package health

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	expect := func(name string, now time.Time, healthy bool, fragment string) {
		t.Helper()
		ok, msg := Check(path, now)
		if ok != healthy || !strings.Contains(msg, fragment) {
			t.Fatalf("%s: got %v %q, want %v containing %q", name, ok, msg, healthy, fragment)
		}
	}

	expect("missing file", time.Now(), false, "no status file")

	tr := NewTracker(path, "1.2.3", time.Minute, zerolog.Nop())
	write := func() {
		t.Helper()
		if err := tr.write(); err != nil {
			t.Fatal(err)
		}
	}
	write()
	now := time.Now()
	expect("fresh start", now, true, "cloud not configured")
	expect("fresh start", now, true, "starting")

	// The loop never finished a cycle, long past the budget.
	tr.status.StartedAt = now.Add(-10 * time.Minute)
	write()
	expect("stalled at start", now, false, "stalled")

	// One failure to reach Docker is a blip; two in a row are not.
	tr.ReconcileDone(errors.New("docker unreachable"), true)
	write()
	expect("one docker failure", now, true, "docker unreachable")
	tr.ReconcileDone(errors.New("docker unreachable"), true)
	write()
	expect("two docker failures", now, false, "Docker unreachable")

	// A per-service failure keeps DockTail healthy.
	tr.ReconcileDone(errors.New("service configuration error"), false)
	write()
	expect("service error", now, true, "with errors")

	// The cloud link is reported, never decisive.
	tr.ReconcileDone(nil, false)
	tr.SetCloudSource(func() *Cloud { return &Cloud{State: CloudRejected, Since: now, Reason: "invalid_key"} })
	write()
	expect("cloud rejected", now, true, "cloud rejected")

	// An unreachable tailscaled socket is unhealthy.
	tr.SetTailscaleProbe(func() error { return errors.New("socket gone") })
	write()
	expect("tailscale down", now, false, "socket gone")
	tr.SetTailscaleProbe(func() error { return nil })
	write()
	expect("tailscale back", now, true, "healthy")

	// A process that stopped rewriting the file is unhealthy.
	expect("stale file", now.Add(2*time.Minute), false, "not updated")
}
