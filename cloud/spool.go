package cloud

import (
	"sync"
	"time"
)

// Offline spool bounds. Entries are pre-encoded envelopes, so a replay re-sends the
// exact frame the agent would have sent at observation time — original
// `occurred_at` included. The entry and byte caps are enforced by evicting the
// OLDEST entries: a long outage must not grow memory without bound, and when
// something has to go the most recent failure signals are the ones worth keeping.
//
// 1024 entries is roughly a day of a badly crash-looping host; 1 MiB bounds the log
// excerpts, which dominate the size (proto.MaxLogBytes is 8 KiB each).
//
// spoolMaxAge is the one that matters for behaviour rather than memory. A replayed
// failure signal opens an incident and pages, so it has to still be news: a
// container that broke minutes ago during a cloud blip is, one that broke hours ago
// is not. Beyond the cap the user has already been told — the cloud opens a
// host_down incident and alerts once the agent stops reporting — so replaying that
// far back would add an alert storm to an outage the user knows about. An hour also
// sits well inside the cloud's 24h timestamp clamp, so nothing replayed reads with
// a rewritten time.
const (
	spoolMaxEntries = 1024
	spoolMaxBytes   = 1 << 20
	spoolMaxAge     = time.Hour
)

// spoolEntry is one frame waiting for a connection, with the moment it was
// observed so the age cap can be applied at replay rather than at capture — the
// frame may well be delivered seconds later, and only a replay needs to ask
// whether it is still worth sending.
type spoolEntry struct {
	env []byte
	at  time.Time
}

// spool holds the frames observed while no connection could carry them — the agent
// is disconnected, or this connection's send queue was momentarily full — for
// replay on the next one that will take them.
//
// Only correctness-critical frames are spooled: docker failure events and the log
// excerpts captured on their down edge. Losing one of those loses an incident
// outright, because nothing later re-derives it — a container that dies and
// recovers inside the gap looks like it never broke. Periodic state (snapshots,
// check results, host metrics, heartbeats) is deliberately NOT spooled: the next
// tick replaces it, so replaying a backlog would be noise rather than signal.
//
// The spool lives in memory only. An agent restart is itself a signal the cloud
// sees (the host goes offline, then reconnects), and persisting excerpts would put
// application log tails on disk — a privacy surface the agent does not otherwise
// have.
type spool struct {
	mu      sync.Mutex
	entries []spoolEntry
	bytes   int
	evicted int // entries dropped unsent since the last drain, for the replay log

	// replayMu serializes whole replays. Both the post-accept sequence and the
	// retry loop replay, and two concurrent drains would interleave their batches.
	replayMu sync.Mutex
}

// add appends a pre-encoded envelope observed now. A frame larger than the whole
// byte budget is still kept (it is a single capped frame, never unbounded); it just
// empties the spool behind it.
func (s *spool) add(env []byte) {
	if len(env) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, spoolEntry{env: env, at: time.Now()})
	s.bytes += len(env)
	s.enforceCaps()
}

// enforceCaps evicts the oldest entries until the entry and byte caps hold again.
// Caller holds s.mu.
func (s *spool) enforceCaps() {
	for len(s.entries) > spoolMaxEntries || (s.bytes > spoolMaxBytes && len(s.entries) > 1) {
		s.bytes -= len(s.entries[0].env)
		s.entries[0].env = nil
		s.entries = s.entries[1:]
		s.evicted++
	}
}

// len reports how many frames are waiting, so the common path — nothing was ever
// spooled — skips the replay work entirely.
func (s *spool) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// drain removes every frame still young enough to be worth sending, oldest first,
// and reports how many were dropped unsent: entries evicted by the caps since the
// previous drain plus any that aged out here.
func (s *spool) drain(now time.Time) (envs [][]byte, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.Add(-spoolMaxAge)
	envs = make([][]byte, 0, len(s.entries))
	for _, e := range s.entries {
		if e.at.Before(cutoff) {
			s.evicted++
			continue
		}
		envs = append(envs, e.env)
	}
	dropped = s.evicted
	s.entries, s.bytes, s.evicted = nil, 0, 0
	return envs, dropped
}

// unshift puts undelivered frames back at the front, keeping them older than
// anything spooled since the drain, so a replay that dies halfway through loses
// nothing the next connection could still deliver. They keep the drain's
// observation time, which is close enough: the age cap is an hour and a failed
// replay is seconds old.
func (s *spool) unshift(envs [][]byte, at time.Time) {
	if len(envs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	back := make([]spoolEntry, 0, len(envs)+len(s.entries))
	for _, env := range envs {
		back = append(back, spoolEntry{env: env, at: at})
	}
	s.entries = append(back, s.entries...)
	s.bytes = 0
	for _, e := range s.entries {
		s.bytes += len(e.env)
	}
	s.enforceCaps()
}

// discard throws the spool away. Used when replaying would be pointless: the cloud
// drops events and excerpts for an unmonitored host, so holding them for a
// promotion that may never come only pins memory.
func (s *spool) discard() (dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dropped = len(s.entries)
	s.entries, s.bytes, s.evicted = nil, 0, 0
	return dropped
}
