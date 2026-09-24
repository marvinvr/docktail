// Package health is DockTail's local status surface. The running process keeps
// a small JSON status file current (Tracker), and `docktail health` reads it
// back and turns it into a verdict (Check) — which is what the image's
// HEALTHCHECK runs. Nothing listens on a port: the status never leaves the
// container unless someone with access to it reads the file.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// EnvFile overrides where the status file lives. Both the running process and
// `docktail health` read it, so they always agree inside one container.
const EnvFile = "HEALTH_FILE"

const (
	// writeInterval is how often the status file is rewritten even when nothing
	// changed; its updated_at is how Check tells a live process from a hung one.
	writeInterval = 10 * time.Second
	// staleAfter is how old updated_at may get before Check calls the process hung.
	staleAfter = 60 * time.Second
	// minReconcileBudget floors how long the reconcile loop may go without
	// finishing a cycle before it counts as stalled; the budget is otherwise
	// three intervals.
	minReconcileBudget = 3 * time.Minute
	// dockerFailureLimit is how many reconciles in a row may fail to read the
	// containers from Docker before DockTail is unhealthy; one is a blip.
	dockerFailureLimit = 2
)

// Cloud link states, as reported by the optional DockTail Cloud module.
const (
	CloudConnecting   = "connecting"   // enabled, no connection accepted yet
	CloudConnected    = "connected"    // an accepted connection is up
	CloudDisconnected = "disconnected" // retrying after a lost connection or failed dial
	CloudRejected     = "rejected"     // the cloud refused the connection (see Reason)
	CloudFailed       = "failed"       // the module could not start (see Reason)
)

// Status is the content of the status file.
type Status struct {
	Version                  string    `json:"version"`
	PID                      int       `json:"pid"`
	StartedAt                time.Time `json:"started_at"`
	UpdatedAt                time.Time `json:"updated_at"`
	ReconcileIntervalSeconds float64   `json:"reconcile_interval_seconds"`
	Reconcile                Reconcile `json:"reconcile"`
	// Tailscale is nil when no socket probe is configured.
	Tailscale *Tailscale `json:"tailscale,omitempty"`
	// Cloud is nil when DockTail Cloud is not configured.
	Cloud *Cloud `json:"cloud,omitempty"`
}

// Reconcile is the outcome of the reconcile loop so far.
type Reconcile struct {
	LastRunAt           *time.Time `json:"last_run_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	// DockerFailures counts the latest reconciles in a row that could not read
	// the containers from Docker.
	DockerFailures int `json:"docker_failures"`
}

// Tailscale is the reachability of the tailscaled socket.
type Tailscale struct {
	SocketReachable bool   `json:"socket_reachable"`
	LastError       string `json:"last_error,omitempty"`
}

// Cloud is the DockTail Cloud link state.
type Cloud struct {
	State  string    `json:"state"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason,omitempty"`
}

// Path is the status file location: $HEALTH_FILE, else docktail-health.json in
// the temp directory (/tmp in the image).
func Path() string {
	if p := strings.TrimSpace(os.Getenv(EnvFile)); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), "docktail-health.json")
}

// Tracker collects the running process's state and keeps the status file
// current. Its methods are safe for concurrent use.
type Tracker struct {
	path  string
	log   zerolog.Logger
	nudge chan struct{}

	mu        sync.Mutex
	status    Status
	cloud     func() *Cloud
	tailscale func() error
}

// NewTracker prepares a tracker; nothing is written until Run.
func NewTracker(path, version string, reconcileInterval time.Duration, logger zerolog.Logger) *Tracker {
	return &Tracker{
		path:  path,
		log:   logger,
		nudge: make(chan struct{}, 1),
		status: Status{
			Version:                  version,
			PID:                      os.Getpid(),
			StartedAt:                time.Now().UTC(),
			ReconcileIntervalSeconds: reconcileInterval.Seconds(),
		},
	}
}

// SetCloudSource installs the function that reports the cloud link state. Leave
// it unset when DockTail Cloud is not configured.
func (t *Tracker) SetCloudSource(fn func() *Cloud) {
	t.mu.Lock()
	t.cloud = fn
	t.mu.Unlock()
}

// SetTailscaleProbe installs the function that checks the tailscaled socket
// (nil error = reachable). It runs before every write.
func (t *Tracker) SetTailscaleProbe(fn func() error) {
	t.mu.Lock()
	t.tailscale = fn
	t.mu.Unlock()
}

// ReconcileDone records the outcome of one reconcile cycle. dockerUnreachable
// marks a failure to read the containers from Docker at all, as opposed to a
// failure while applying individual services.
func (t *Tracker) ReconcileDone(err error, dockerUnreachable bool) {
	now := time.Now().UTC()
	t.mu.Lock()
	r := &t.status.Reconcile
	r.LastRunAt = &now
	if err == nil {
		r.LastSuccessAt = &now
		r.LastError = ""
		r.ConsecutiveFailures = 0
	} else {
		r.LastError = err.Error()
		r.ConsecutiveFailures++
	}
	if err != nil && dockerUnreachable {
		r.DockerFailures++
	} else {
		r.DockerFailures = 0
	}
	t.mu.Unlock()
	select {
	case t.nudge <- struct{}{}:
	default:
	}
}

// Run writes the status file now, after every reconcile and every
// writeInterval, until ctx ends; it then removes the file so nothing reads a
// stopped process as healthy. A failed write is logged once, not per attempt.
func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(writeInterval)
	defer ticker.Stop()
	defer func() { _ = os.Remove(t.path) }()
	failing := false
	for {
		if err := t.write(); err != nil {
			if !failing {
				t.log.Warn().Err(err).Str("path", t.path).
					Msgf("Cannot write the health status file, so `docktail health` (the container healthcheck) will report unhealthy; point %s at a writable path", EnvFile)
			}
			failing = true
		} else {
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-t.nudge:
		}
	}
}

func (t *Tracker) snapshot() Status {
	t.mu.Lock()
	s := t.status
	cloud, probe := t.cloud, t.tailscale
	t.mu.Unlock()
	if cloud != nil {
		s.Cloud = cloud()
	}
	if probe != nil {
		s.Tailscale = &Tailscale{SocketReachable: true}
		if err := probe(); err != nil {
			s.Tailscale = &Tailscale{LastError: err.Error()}
		}
	}
	s.UpdatedAt = time.Now().UTC()
	return s
}

// write replaces the status file atomically (temp file + rename), so a
// concurrent `docktail health` never reads a half-written file.
func (t *Tracker) write() error {
	data, err := json.MarshalIndent(t.snapshot(), "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.path)
	tmp, err := os.CreateTemp(dir, ".docktail-health-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(data, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), t.path)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
	}
	return werr
}

// Check reads the status file at path and decides whether DockTail is healthy,
// returning a one-line explanation either way.
//
// Healthy means DockTail can do its job: the process is alive (the file was
// rewritten recently), the reconcile loop keeps finishing cycles (within three
// reconcile intervals, at least three minutes), Docker answers, and the
// tailscaled socket accepts connections. A cycle that fails on individual services (a
// label conflict, a service the tailnet refuses) is reported but stays healthy:
// that is one container's configuration, and DockTail keeps serving the rest.
//
// The DockTail Cloud link is reported but never decides the verdict: an outage
// or a rejection there does not stop DockTail from serving containers, and a
// restart triggered by an unhealthy status (Swarm, autoheal) would drain every
// service without fixing anything on the cloud side.
func Check(path string, now time.Time) (bool, string) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator-configured status file this binary writes itself
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Sprintf("unhealthy: no status file at %s (DockTail is not running, is still starting, or cannot write it; see %s)", path, EnvFile)
		}
		return false, fmt.Sprintf("unhealthy: cannot read %s: %v", path, err)
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return false, fmt.Sprintf("unhealthy: cannot parse %s: %v", path, err)
	}
	if age := now.Sub(s.UpdatedAt); age > staleAfter {
		return false, fmt.Sprintf("unhealthy: status not updated for %s (DockTail is hung or stopped)", roundAge(age))
	}

	budget := 3 * time.Duration(s.ReconcileIntervalSeconds*float64(time.Second))
	if budget < minReconcileBudget {
		budget = minReconcileBudget
	}
	r := s.Reconcile
	cloud := cloudSummary(s.Cloud, now)

	if s.Tailscale != nil && !s.Tailscale.SocketReachable {
		return false, fmt.Sprintf("unhealthy: tailscaled socket unreachable: %s; %s", s.Tailscale.LastError, cloud)
	}
	if r.DockerFailures >= dockerFailureLimit {
		return false, fmt.Sprintf("unhealthy: Docker unreachable in the last %d reconciles%s; %s", r.DockerFailures, lastErr(r), cloud)
	}
	if r.LastRunAt == nil {
		if since := now.Sub(s.StartedAt); since > budget {
			return false, fmt.Sprintf("unhealthy: no reconcile finished in %s since start (reconcile loop stalled); %s", roundAge(since), cloud)
		}
		return true, "healthy: starting, no reconcile finished yet; " + cloud
	}
	if age := now.Sub(*r.LastRunAt); age > budget {
		return false, fmt.Sprintf("unhealthy: no reconcile finished in %s (reconcile loop stalled); %s", roundAge(age), cloud)
	}
	if r.ConsecutiveFailures > 0 {
		return true, fmt.Sprintf("healthy, with errors: last %d reconcile(s) failed%s; %s", r.ConsecutiveFailures, lastErr(r), cloud)
	}
	return true, fmt.Sprintf("healthy: last reconcile %s ago; %s", roundAge(now.Sub(*r.LastRunAt)), cloud)
}

func lastErr(r Reconcile) string {
	if r.LastError == "" {
		return ""
	}
	return " (last error: " + r.LastError + ")"
}

func cloudSummary(c *Cloud, now time.Time) string {
	if c == nil {
		return "cloud not configured"
	}
	s := "cloud " + c.State
	if !c.Since.IsZero() {
		s += " for " + roundAge(now.Sub(c.Since))
	}
	if c.Reason != "" {
		s += " (" + c.Reason + ")"
	}
	return s
}

func roundAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
