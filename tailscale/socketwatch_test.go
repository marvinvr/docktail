package tailscale

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// listenUnix creates a unix socket at path and returns a closer.
func listenUnix(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// socketPath keeps the path short: a unix socket address is limited to ~104
// bytes, and t.TempDir() on macOS is already long.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dtw")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func TestProbeSocketReachable(t *testing.T) {
	path := socketPath(t)
	listenUnix(t, path)

	c := &Client{socketPath: path}
	if err := c.ProbeSocket(); err != nil {
		t.Fatalf("expected reachable socket, got %v", err)
	}
}

func TestProbeSocketMissing(t *testing.T) {
	c := &Client{socketPath: socketPath(t)}

	err := c.ProbeSocket()
	if err == nil {
		t.Fatal("expected an error for a socket that does not exist")
	}
	if !IsSocketUnreachable(err) {
		t.Fatalf("expected SocketUnreachableError, got %T: %v", err, err)
	}
	var sue *SocketUnreachableError
	if !errors.As(err, &sue) || !sue.Missing {
		t.Fatalf("expected Missing=true, got %+v", sue)
	}
}

// A socket file that outlives its listener is the sidecar variant of the bug:
// the path still resolves, but nothing is accepting on it.
func TestProbeSocketStaleFile(t *testing.T) {
	path := socketPath(t)

	// Go unlinks a unix socket when its listener closes, so opt out to leave the
	// file behind with nothing accepting on it.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file should still exist: %v", err)
	}

	c := &Client{socketPath: path}
	err = c.ProbeSocket()
	if err == nil {
		t.Fatal("expected an error for a socket with no listener")
	}
	var sue *SocketUnreachableError
	if !errors.As(err, &sue) {
		t.Fatalf("expected SocketUnreachableError, got %T: %v", err, err)
	}
	if sue.Missing {
		t.Fatal("expected Missing=false for a socket file that exists but refuses connections")
	}
}

func TestProbeSocketEmptyPathIsNoop(t *testing.T) {
	c := &Client{}
	if err := c.ProbeSocket(); err != nil {
		t.Fatalf("expected no error for an empty socket path, got %v", err)
	}
}

// newWatchdog builds a watchdog and records whether onLost fired.
func newWatchdog(t *testing.T, path string, grace time.Duration) (*SocketWatchdog, *int, *time.Duration) {
	t.Helper()
	calls := 0
	var downFor time.Duration
	c := &Client{socketPath: path}
	w := c.NewSocketWatchdog(
		SocketWatchdogConfig{Enabled: true, Grace: grace},
		func(err error, d time.Duration) { calls++; downFor = d },
	)
	return w, &calls, &downFor
}

func TestWatchdogTripsAfterGrace(t *testing.T) {
	path := socketPath(t)
	ln := listenUnix(t, path)

	w, calls, downFor := newWatchdog(t, path, 90*time.Second)
	t0 := time.Now()

	// Healthy: arms the watchdog.
	if done := w.check(t0); done {
		t.Fatal("watchdog should not be done while the socket is reachable")
	}

	// The socket goes away and never returns.
	_ = ln.Close()
	_ = os.Remove(path)

	if done := w.check(t0.Add(10 * time.Second)); done {
		t.Fatal("watchdog should still be waiting inside the grace period")
	}
	if *calls != 0 {
		t.Fatalf("onLost fired early (%d calls)", *calls)
	}

	if done := w.check(t0.Add(100 * time.Second)); !done {
		t.Fatal("watchdog should be done once the grace period has elapsed")
	}
	if *calls != 1 {
		t.Fatalf("expected onLost exactly once, got %d", *calls)
	}
	if *downFor < 90*time.Second {
		t.Fatalf("expected downFor >= grace, got %v", *downFor)
	}
}

// A tailscaled restart that keeps the mount intact drops the socket for a second
// or two. That must not trigger an exit.
func TestWatchdogToleratesBriefOutage(t *testing.T) {
	path := socketPath(t)
	ln := listenUnix(t, path)

	w, calls, _ := newWatchdog(t, path, 90*time.Second)
	t0 := time.Now()
	w.check(t0)

	_ = ln.Close()
	_ = os.Remove(path)
	w.check(t0.Add(5 * time.Second))

	listenUnix(t, path)
	if done := w.check(t0.Add(10 * time.Second)); done {
		t.Fatal("watchdog should not be done after the socket came back")
	}

	// Well past the grace period, but the clock restarted when it recovered.
	if done := w.check(t0.Add(200 * time.Second)); done {
		t.Fatal("watchdog must not trip on a socket that recovered")
	}
	if *calls != 0 {
		t.Fatalf("expected onLost never to fire, got %d calls", *calls)
	}
}

// DockTail starting before tailscaled must wait, not exit: a restart would not
// make the socket appear any sooner.
func TestWatchdogDoesNotArmUntilSocketSeen(t *testing.T) {
	path := socketPath(t)

	w, calls, _ := newWatchdog(t, path, 30*time.Second)
	t0 := time.Now()

	for _, d := range []time.Duration{0, 10, 60, 600} {
		if done := w.check(t0.Add(d * time.Second)); done {
			t.Fatalf("watchdog tripped at +%vs without ever seeing the socket", d)
		}
	}
	if *calls != 0 {
		t.Fatalf("expected onLost never to fire, got %d calls", *calls)
	}

	// Once it appears the watchdog arms, and a later loss does trip it.
	ln := listenUnix(t, path)
	w.check(t0.Add(700 * time.Second))
	_ = ln.Close()
	_ = os.Remove(path)
	w.check(t0.Add(701 * time.Second))
	if done := w.check(t0.Add(800 * time.Second)); !done {
		t.Fatal("watchdog should trip after the socket was seen and then lost")
	}
	if *calls != 1 {
		t.Fatalf("expected onLost exactly once, got %d", *calls)
	}
}

func TestWatchdogReportsOnce(t *testing.T) {
	path := socketPath(t)
	ln := listenUnix(t, path)

	w, calls, _ := newWatchdog(t, path, 10*time.Second)
	t0 := time.Now()
	w.check(t0)
	_ = ln.Close()
	_ = os.Remove(path)
	w.check(t0.Add(1 * time.Second))

	for i := 0; i < 5; i++ {
		w.check(t0.Add(time.Duration(60+i) * time.Second))
	}
	if *calls != 1 {
		t.Fatalf("expected onLost exactly once, got %d", *calls)
	}
}

func TestWatchdogRunDisabled(t *testing.T) {
	path := socketPath(t)
	c := &Client{socketPath: path}

	for name, cfg := range map[string]SocketWatchdogConfig{
		"disabled":   {Enabled: false, Grace: time.Second},
		"zero grace": {Enabled: true, Grace: 0},
	} {
		t.Run(name, func(t *testing.T) {
			called := false
			w := c.NewSocketWatchdog(cfg, func(error, time.Duration) { called = true })
			done := make(chan struct{})
			go func() { w.Run(t.Context()); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run should return immediately when the watchdog is off")
			}
			if called {
				t.Fatal("onLost must not fire when the watchdog is off")
			}
		})
	}
}
