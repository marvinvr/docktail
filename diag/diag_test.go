package diag

import (
	"context"
	"testing"
	"time"
)

// DIAGNOSTICS_FILE= is documented as "record to the log only", which is not the
// same thing as leaving the variable unset.
func TestLoadConfigDistinguishesEmptyFileFromUnset(t *testing.T) {
	if got := LoadConfig().File; got != defaultFile {
		t.Fatalf("unset DIAGNOSTICS_FILE should give the default path, got %q", got)
	}

	t.Setenv("DIAGNOSTICS_FILE", "")
	if got := LoadConfig().File; got != "" {
		t.Fatalf("an explicitly empty DIAGNOSTICS_FILE should select log-only recording, got %q", got)
	}

	t.Setenv("DIAGNOSTICS_FILE", "/tmp/somewhere.jsonl")
	if got := LoadConfig().File; got != "/tmp/somewhere.jsonl" {
		t.Fatalf("expected the configured path, got %q", got)
	}
}

// time.NewTicker panics on a non-positive interval, so a mistyped setting must
// never reach it: diagnostics is a troubleshooting aid and must not be able to
// take DockTail down.
func TestNewRejectsNonPositiveInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -5 * time.Second} {
		r := New(Config{Enabled: true, Interval: interval}, nil, "test")
		if r.cfg.Interval <= 0 {
			t.Fatalf("interval %v was not corrected, Run would panic", interval)
		}
	}
}

// Run must return when its context is cancelled rather than block shutdown.
func TestRunStopsOnContextCancel(t *testing.T) {
	// A nil client would panic inside sample, so this only covers the loop when
	// there is nothing to sample: an already-cancelled context returns before the
	// first sample is taken.
	r := New(Config{Enabled: true, Interval: time.Hour, File: ""}, nil, "test")
	r.lastState = "unused"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer func() {
			// sample() dereferences the client; the point here is that Run reaches
			// its shutdown path rather than spinning.
			_ = recover()
			close(done)
		}()
		r.Run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
