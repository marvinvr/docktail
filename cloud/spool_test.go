package cloud

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func held(s *spool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, string(e.env))
	}
	return out
}

func drained(t *testing.T, s *spool) ([]string, int) {
	t.Helper()
	envs, dropped := s.drain(time.Now())
	out := make([]string, 0, len(envs))
	for _, env := range envs {
		out = append(out, string(env))
	}
	return out, dropped
}

func TestSpoolDrainsInObservationOrder(t *testing.T) {
	var s spool
	s.add([]byte("die"))
	s.add([]byte("excerpt"))
	s.add([]byte("start"))

	if got := s.len(); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	got, dropped := drained(t, &s)
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	want := []string{"die", "excerpt", "start"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("drained %v, want %v", got, want)
	}
	if s.len() != 0 {
		t.Errorf("spool not empty after drain")
	}
}

func TestSpoolEvictsOldestAtEntryCap(t *testing.T) {
	var s spool
	for i := 0; i < spoolMaxEntries+10; i++ {
		s.add([]byte(fmt.Sprintf("e%d", i)))
	}
	got, dropped := drained(t, &s)
	if len(got) != spoolMaxEntries {
		t.Fatalf("held %d frames, want %d", len(got), spoolMaxEntries)
	}
	if dropped != 10 {
		t.Errorf("dropped = %d, want 10", dropped)
	}
	// The newest failure signals are the ones worth keeping.
	if want := "e10"; got[0] != want {
		t.Errorf("oldest kept = %q, want %q", got[0], want)
	}
	if want := fmt.Sprintf("e%d", spoolMaxEntries+9); got[len(got)-1] != want {
		t.Errorf("newest kept = %q, want %q", got[len(got)-1], want)
	}
}

func TestSpoolEvictsOldestAtByteCap(t *testing.T) {
	var s spool
	// Exactly four frames fit the byte budget, so the fifth and sixth each evict one.
	for i := 0; i < 6; i++ {
		frame := bytes.Repeat([]byte("x"), spoolMaxBytes/4)
		frame[len(frame)-1] = byte('0' + i)
		s.add(frame)
	}
	envs, dropped := s.drain(time.Now())
	if len(envs) != 4 {
		t.Fatalf("held %d frames, want 4", len(envs))
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if got := envs[0][len(envs[0])-1]; got != '2' {
		t.Errorf("oldest kept ends in %q, want '2'", got)
	}
}

func TestSpoolKeepsAnOversizedFrame(t *testing.T) {
	var s spool
	s.add([]byte("small"))
	s.add(bytes.Repeat([]byte("y"), spoolMaxBytes*2))
	envs, _ := s.drain(time.Now())
	if len(envs) != 1 {
		t.Fatalf("held %d frames, want 1", len(envs))
	}
	if len(envs[0]) != spoolMaxBytes*2 {
		t.Errorf("kept a frame of %d bytes, want the oversized one", len(envs[0]))
	}
}

func TestSpoolDrainDropsFramesPastTheAgeCap(t *testing.T) {
	var s spool
	s.add([]byte("stale"))
	s.add([]byte("fresh"))
	s.mu.Lock()
	s.entries[0].at = time.Now().Add(-spoolMaxAge - time.Minute)
	s.mu.Unlock()

	envs, dropped := s.drain(time.Now())
	if len(envs) != 1 || string(envs[0]) != "fresh" {
		t.Fatalf("drained %d frames (%q), want just the fresh one", len(envs), envs)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

func TestSpoolUnshiftKeepsUndeliveredFramesOldest(t *testing.T) {
	var s spool
	s.add([]byte("a"))
	s.add([]byte("b"))
	s.add([]byte("c"))
	envs, _ := s.drain(time.Now())

	// "a" went out, the connection died, and a new event arrived meanwhile.
	s.add([]byte("d"))
	s.unshift(envs[1:], time.Now())

	want := []string{"b", "c", "d"}
	if got := held(&s); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("holding %v, want %v", got, want)
	}
	if s.bytes != 3 {
		t.Errorf("bytes = %d, want 3", s.bytes)
	}
}

func TestSpoolDiscardEmpties(t *testing.T) {
	var s spool
	s.add([]byte("a"))
	s.add([]byte("b"))
	if n := s.discard(); n != 2 {
		t.Fatalf("discard = %d, want 2", n)
	}
	if s.len() != 0 || s.bytes != 0 {
		t.Errorf("spool not empty after discard: %d frames, %d bytes", s.len(), s.bytes)
	}
}
