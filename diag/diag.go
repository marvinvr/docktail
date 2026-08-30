// Package diag implements DockTail's opt-in diagnostics recorder.
//
// It exists for one reason: when a Tailscale Service goes offline while the
// container is healthy and DockTail's logs look clean (issue #72), there is
// currently no record of what the node's hosting state actually was at the
// moment it broke. A Service is hosted only when the serve config AND
// prefs.AdvertiseServices agree, and DockTail's reconciler only ever reads the
// first of those, so the interesting half is invisible in normal operation.
//
// The recorder samples both halves on a short interval and appends a JSON line
// whenever the state changes (plus a periodic heartbeat so gaps in the file are
// distinguishable from a stopped agent). It is completely inert unless
// DIAGNOSTICS=true.
package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/tailscale"
)

// Config controls the recorder. Zero value is disabled.
type Config struct {
	Enabled bool
	// File is where JSON lines are appended. Empty means "log only".
	File string
	// Interval is how often state is sampled.
	Interval time.Duration
	// Heartbeat forces a record even when nothing changed, so a quiet period is
	// distinguishable from a dead recorder.
	Heartbeat time.Duration
}

const (
	defaultFile      = "/diagnostics/docktail-diagnostics.jsonl"
	defaultInterval  = 10 * time.Second
	defaultHeartbeat = 10 * time.Minute
)

// LoadConfig reads the recorder configuration from the environment.
func LoadConfig() Config {
	return Config{
		Enabled: envBool("DIAGNOSTICS", false),
		// Looked up rather than read with a default, so that an explicitly empty
		// DIAGNOSTICS_FILE selects log-only recording instead of the default path.
		File:      envPath("DIAGNOSTICS_FILE", defaultFile),
		Interval:  envDuration("DIAGNOSTICS_INTERVAL", defaultInterval),
		Heartbeat: envDuration("DIAGNOSTICS_HEARTBEAT", defaultHeartbeat),
	}
}

// Enabled reports whether diagnostics recording is switched on.
func Enabled() bool { return envBool("DIAGNOSTICS", false) }

// record is one sample, written as a single JSON line.
type record struct {
	Timestamp string                   `json:"ts"`
	Reason    string                   `json:"reason"` // start | change | heartbeat | error
	Agent     string                   `json:"agent_version,omitempty"`
	Backend   string                   `json:"backend_state,omitempty"`
	NodeID    string                   `json:"node_id,omitempty"`
	Health    []string                 `json:"daemon_health,omitempty"`
	Advertise []string                 `json:"advertise_services"`
	Services  []tailscale.ServiceState `json:"services"`
	// VIPFingerprint is the tuple tailscaled hashes into Hostinfo.ServicesHash.
	// Control is only notified when this changes.
	VIPFingerprint string   `json:"vip_fingerprint"`
	Anomalies      []string `json:"anomalies,omitempty"`
	Error          string   `json:"error,omitempty"`
}

// Recorder samples the node's Tailscale hosting state on an interval.
type Recorder struct {
	cfg     Config
	ts      *tailscale.Client
	version string

	out       *os.File
	lastState string
	lastWrite time.Time
	// warned tracks which anomalies have already been logged, so a persistent
	// problem produces one warning rather than one per interval.
	warned map[string]bool
}

// New creates a recorder and opens the output file when one is configured.
// A file that cannot be opened is not fatal: recording degrades to log-only.
func New(cfg Config, ts *tailscale.Client, version string) *Recorder {
	// time.NewTicker panics on a non-positive interval, so a DIAGNOSTICS_INTERVAL
	// of `0s` must never reach it. A diagnostics setting is not worth crashing
	// DockTail over.
	if cfg.Interval <= 0 {
		log.Warn().Dur("interval", cfg.Interval).Dur("using", defaultInterval).
			Msg("Diagnostics: sampling interval must be positive, falling back to the default")
		cfg.Interval = defaultInterval
	}
	if cfg.Heartbeat < 0 {
		cfg.Heartbeat = defaultHeartbeat
	}

	r := &Recorder{cfg: cfg, ts: ts, version: version, warned: make(map[string]bool)}

	if cfg.File == "" {
		return r
	}
	if err := os.MkdirAll(filepath.Dir(cfg.File), 0o755); err != nil {
		log.Warn().Err(err).Str("file", cfg.File).
			Msg("Diagnostics: could not create output directory, recording to the log only")
		return r
	}
	f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Warn().Err(err).Str("file", cfg.File).
			Msg("Diagnostics: could not open output file, recording to the log only")
		return r
	}
	r.out = f
	return r
}

// Run samples until the context is cancelled.
func (r *Recorder) Run(ctx context.Context) {
	defer func() {
		if r.out != nil {
			_ = r.out.Close()
		}
	}()

	log.Info().
		Str("file", r.cfg.File).
		Dur("interval", r.cfg.Interval).
		Dur("heartbeat", r.cfg.Heartbeat).
		Msg("Diagnostics recording enabled")

	r.sample(ctx, "start")

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// The context that ended the loop is already cancelled, so reusing it
			// here would fail every CLI call and turn the closing record into an
			// error instead of the documented `stop`.
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r.sample(stopCtx, "stop")
			cancel()
			return
		case <-ticker.C:
			r.sample(ctx, "change")
		}
	}
}

// sample reads the current state and writes a record when it changed, when the
// heartbeat is due, or when the reason is not a routine tick.
func (r *Recorder) sample(ctx context.Context, reason string) {
	rec := record{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Reason:    reason,
		Agent:     r.version,
	}

	advertised, err := r.ts.AdvertisedServices(ctx)
	if err != nil {
		rec.Error = err.Error()
		rec.Reason = "error"
		r.write(rec)
		return
	}
	rec.Advertise = advertised

	serveConfig, err := r.ts.ServeConfigSnapshot(ctx)
	if err != nil {
		rec.Error = err.Error()
		rec.Reason = "error"
		r.write(rec)
		return
	}

	if daemon, err := r.ts.DaemonState(ctx); err == nil {
		rec.Backend = daemon.BackendState
		rec.NodeID = daemon.NodeID
		rec.Health = daemon.Health
	}

	rec.Services = tailscale.ServiceStates(serveConfig, advertised)
	rec.VIPFingerprint = tailscale.VIPFingerprint(rec.Services)
	rec.Anomalies = anomalies(rec.Services)

	r.reportAnomalies(rec.Anomalies)

	// The fingerprint alone is not enough to decide "changed": a backend that
	// moved to a new container IP leaves it untouched (by design, that is what
	// the hash does too), and that move is exactly what we want recorded.
	state := stateKey(rec)
	changed := state != r.lastState
	heartbeatDue := r.cfg.Heartbeat > 0 && time.Since(r.lastWrite) >= r.cfg.Heartbeat

	switch {
	case reason == "start" || reason == "stop":
		// always record
	case changed:
		rec.Reason = "change"
	case heartbeatDue:
		rec.Reason = "heartbeat"
	default:
		return
	}

	r.lastState = state
	r.write(rec)
}

// stateKey is everything a record must differ in to count as a change.
func stateKey(rec record) string {
	var sb strings.Builder
	sb.WriteString(rec.Backend)
	sb.WriteByte('|')
	sb.WriteString(rec.VIPFingerprint)
	sb.WriteByte('|')
	for _, s := range rec.Services {
		sb.WriteString(s.Name)
		sb.WriteByte('=')
		sb.WriteString(s.Destination)
		sb.WriteByte(';')
	}
	sb.WriteByte('|')
	sb.WriteString(strings.Join(rec.Health, ","))
	return sb.String()
}

func anomalies(states []tailscale.ServiceState) []string {
	var out []string
	for _, s := range states {
		if a := s.Anomaly(); a != "" {
			out = append(out, fmt.Sprintf("%s: %s", s.Name, a))
		}
	}
	sort.Strings(out)
	return out
}

// reportAnomalies logs each anomaly once when it appears and once when it
// clears, so the normal log carries the signal even without the JSONL file.
func (r *Recorder) reportAnomalies(current []string) {
	seen := make(map[string]bool, len(current))
	for _, a := range current {
		seen[a] = true
		if !r.warned[a] {
			r.warned[a] = true
			log.Warn().
				Str("anomaly", a).
				Msg("Diagnostics: service hosting state is inconsistent; the service may be offline to the tailnet while DockTail reports it as healthy")
		}
	}
	for a := range r.warned {
		if !seen[a] {
			delete(r.warned, a)
			log.Info().Str("anomaly", a).Msg("Diagnostics: previously reported inconsistency has cleared")
		}
	}
}

func (r *Recorder) write(rec record) {
	r.lastWrite = time.Now()

	line, err := json.Marshal(rec)
	if err != nil {
		log.Warn().Err(err).Msg("Diagnostics: could not encode record")
		return
	}

	log.Debug().
		Str("reason", rec.Reason).
		Str("vip_fingerprint", rec.VIPFingerprint).
		Strs("advertise_services", rec.Advertise).
		Msg("Diagnostics sample")

	if r.out == nil {
		return
	}
	if _, err := r.out.Write(append(line, '\n')); err != nil {
		log.Warn().Err(err).Msg("Diagnostics: could not write record")
	}
}

// envPath differs from the other helpers in honouring an explicitly empty value:
// `DIAGNOSTICS_FILE=` is how recording to the log only is requested, which is not
// the same thing as leaving the variable unset.
func envPath(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			return parsed
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if parsed, err := time.ParseDuration(v); err == nil {
			return parsed
		}
	}
	return def
}
