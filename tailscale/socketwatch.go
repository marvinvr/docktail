package tailscale

import (
	"context"
	"errors"
	"net"
	"os"
	"time"

	"github.com/rs/zerolog/log"
)

// SocketUnreachableError describes why the tailscaled LocalAPI socket could not
// be reached. It exists so callers can tell "the socket file is gone from our
// mount namespace" apart from "the daemon is not accepting connections".
type SocketUnreachableError struct {
	Path string
	// Missing is true when the socket path does not exist, false when it exists
	// but the connection was refused (a stale socket inode, or a stopped daemon).
	Missing bool
	Err     error
}

func (e *SocketUnreachableError) Error() string {
	if e.Missing {
		return "tailscaled socket " + e.Path + " does not exist: " + e.Err.Error()
	}
	return "tailscaled socket " + e.Path + " is not accepting connections: " + e.Err.Error()
}

func (e *SocketUnreachableError) Unwrap() error { return e.Err }

// ProbeSocket reports whether the tailscaled LocalAPI socket is reachable from
// this process's mount namespace. It dials rather than only stat-ing, because a
// bind-mounted socket *file* survives its daemon: the path keeps resolving to an
// unlinked inode and every connection to it is refused.
//
// A nil error means a connection was established and immediately closed.
func (c *Client) ProbeSocket() error {
	if c.socketPath == "" {
		return nil
	}

	if _, err := os.Stat(c.socketPath); err != nil {
		return &SocketUnreachableError{Path: c.socketPath, Missing: os.IsNotExist(err), Err: err}
	}

	conn, err := net.DialTimeout("unix", c.socketPath, 2*time.Second)
	if err != nil {
		return &SocketUnreachableError{Path: c.socketPath, Err: err}
	}
	_ = conn.Close()
	return nil
}

// SocketWatchdogConfig configures the watchdog.
type SocketWatchdogConfig struct {
	// Enabled switches the watchdog on. When false, Run returns immediately.
	Enabled bool
	// Grace is how long the socket may stay unreachable before the watchdog
	// gives up. A daemon restart takes a second or two, so this must be
	// comfortably longer than that.
	Grace time.Duration
	// Interval is how often the socket is probed.
	Interval time.Duration
}

// SocketWatchdog detects a tailscaled socket that is unreachable for longer than
// a restart would explain, and reports it exactly once.
//
// This exists because of a failure mode DockTail cannot recover from in-process
// (issue #72). When tailscaled runs on the host under systemd, its unit declares
// `RuntimeDirectory=tailscale` with the default `RuntimeDirectoryPreserve=no`,
// so systemd deletes /run/tailscale on stop and creates a *new* directory on
// start. A container that bind-mounts that directory is pinned to the old,
// now-unlinked inode: the socket never reappears inside the container, no matter
// how long DockTail waits or how often it retries. The same happens when a
// Tailscale sidecar container is recreated and the socket is shared through a
// host path rather than a named volume.
//
// The mount is only re-resolved when the container starts, so the sole in-process
// remedy is to exit and let the container's restart policy re-create it.
//
// The watchdog arms only after the socket has been reachable at least once, so a
// DockTail that simply started before tailscaled waits instead of exiting.
type SocketWatchdog struct {
	client *Client
	cfg    SocketWatchdogConfig
	// onLost is called once, from Run's goroutine, when the socket has been
	// unreachable for longer than Grace.
	onLost func(err error, downFor time.Duration)

	armed    bool
	lostAt   time.Time
	lastErr  error
	reported bool
}

// NewSocketWatchdog creates a watchdog for this client's socket path. onLost is
// invoked at most once per watchdog.
func (c *Client) NewSocketWatchdog(cfg SocketWatchdogConfig, onLost func(err error, downFor time.Duration)) *SocketWatchdog {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	return &SocketWatchdog{client: c, cfg: cfg, onLost: onLost}
}

// Run probes until the context is cancelled or the socket is declared lost.
func (w *SocketWatchdog) Run(ctx context.Context) {
	if !w.cfg.Enabled || w.cfg.Grace <= 0 || w.client.socketPath == "" {
		return
	}

	log.Debug().
		Str("socket", w.client.socketPath).
		Dur("grace", w.cfg.Grace).
		Msg("Tailscale socket watchdog started")

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if w.check(time.Now()) {
				return
			}
		}
	}
}

// check evaluates one probe and reports whether the watchdog is done. It takes
// the current time so tests do not have to sleep through a grace period.
func (w *SocketWatchdog) check(now time.Time) bool {
	err := w.client.ProbeSocket()

	if err == nil {
		if !w.armed {
			w.armed = true
			log.Debug().Str("socket", w.client.socketPath).Msg("Tailscale socket watchdog armed")
		}
		if !w.lostAt.IsZero() {
			log.Info().
				Str("socket", w.client.socketPath).
				Dur("unreachable_for", now.Sub(w.lostAt)).
				Msg("Tailscale socket is reachable again")
			w.lostAt = time.Time{}
			w.lastErr = nil
		}
		return false
	}

	// Never reachable since startup: DockTail may simply have started before
	// tailscaled. Waiting is correct here; restarting would not help.
	if !w.armed {
		return false
	}

	if w.lostAt.IsZero() {
		w.lostAt = now
		w.lastErr = err
		log.Warn().Err(err).
			Str("socket", w.client.socketPath).
			Dur("grace", w.cfg.Grace).
			Msg("Tailscale socket became unreachable; waiting for it to come back")
		return false
	}
	w.lastErr = err

	downFor := now.Sub(w.lostAt)
	if downFor < w.cfg.Grace {
		return false
	}

	if w.reported {
		return true
	}
	w.reported = true
	if w.onLost != nil {
		w.onLost(w.lastErr, downFor)
	}
	return true
}

// IsSocketUnreachable reports whether err came from an unreachable tailscaled
// socket.
func IsSocketUnreachable(err error) bool {
	var target *SocketUnreachableError
	return errors.As(err, &target)
}
