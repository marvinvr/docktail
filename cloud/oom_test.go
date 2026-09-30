package cloud

import (
	"testing"
	"time"
)

func TestTakeRecentOOMTiesDieToPrecedingOOM(t *testing.T) {
	c := &Collector{oomSeen: map[string]time.Time{}}
	t0 := time.UnixMilli(1_700_000_000_000)

	c.noteOOM("abc", t0)
	if !c.takeRecentOOM("abc", t0.Add(50*time.Millisecond)) {
		t.Fatal("die right after oom should be tied to it")
	}
	if c.takeRecentOOM("abc", t0.Add(100*time.Millisecond)) {
		t.Fatal("an oom must be consumed by the first die")
	}
}

func TestTakeRecentOOMIgnoresOtherContainersAndStaleOOMs(t *testing.T) {
	c := &Collector{oomSeen: map[string]time.Time{}}
	t0 := time.UnixMilli(1_700_000_000_000)

	c.noteOOM("abc", t0)
	if c.takeRecentOOM("other", t0.Add(time.Second)) {
		t.Fatal("die of another container must not be tied to this oom")
	}
	if c.takeRecentOOM("abc", t0.Add(oomDieWindow+time.Second)) {
		t.Fatal("die long after a process-level oom is not the OOM kill")
	}
}

func TestNoteOOMPrunesStaleEntries(t *testing.T) {
	c := &Collector{oomSeen: map[string]time.Time{}}
	t0 := time.UnixMilli(1_700_000_000_000)

	c.noteOOM("old", t0)
	c.noteOOM("new", t0.Add(oomDieWindow+time.Second))
	if _, ok := c.oomSeen["old"]; ok {
		t.Fatal("stale oom entry should be pruned")
	}
}
