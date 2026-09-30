package cloud

import (
	"fmt"
	"sync"
	"time"

	"github.com/marvinvr/docktail/cloud/proto"
)

// Link states reported by Collector.LinkStatus, for DockTail's local health
// status (see ../health).
const (
	LinkConnecting   = "connecting"   // no connection accepted yet
	LinkConnected    = "connected"    // an accepted connection is up
	LinkDisconnected = "disconnected" // lost the connection or cannot reach the cloud; retrying
	LinkRejected     = "rejected"     // the cloud refused the last attempt (Reason names why)
)

// clockSkewThreshold is how far this host's clock may drift from the cloud's
// before the agent warns: timestamps the agent sends (docker events, check
// results) would otherwise land out of order with the cloud's own.
const clockSkewThreshold = 60 * time.Second

// clockSkewWarnEvery throttles the skew warning across reconnects.
const clockSkewWarnEvery = time.Hour

// LinkStatus is the cloud connection state at a point in time.
type LinkStatus struct {
	State  string
	Since  time.Time // when State was entered
	Reason string    // rejection reason or last connection error, if any
}

// linkTracker records the link state. It has its own lock so reading it never
// waits on the collector's.
type linkTracker struct {
	mu           sync.Mutex
	status       LinkStatus
	lastSkewWarn time.Time
}

// LinkStatus reports the current cloud connection state.
func (c *Collector) LinkStatus() LinkStatus {
	c.link.mu.Lock()
	defer c.link.mu.Unlock()
	if c.link.status.State == "" {
		return LinkStatus{State: LinkConnecting, Since: c.createdAt}
	}
	return c.link.status
}

func (c *Collector) setLink(state, reason string) {
	c.link.mu.Lock()
	defer c.link.mu.Unlock()
	if c.link.status.State != state {
		c.link.status.Since = time.Now()
	}
	c.link.status.State = state
	c.link.status.Reason = reason
}

// noteDial records the outcome of a dial that did not produce a connection. A
// 401/403 upgrade is the cloud (or something in front of it) refusing this
// agent; anything else is an ordinary failure that is retried.
func (c *Collector) noteDial(err error) {
	if err == nil {
		return
	}
	var de *dialError
	if asDialError(err, &de) && (de.statusCode == 401 || de.statusCode == 403) {
		c.setLink(LinkRejected, fmt.Sprintf("http_%d", de.statusCode))
		return
	}
	c.setLink(LinkDisconnected, err.Error())
}

// noteHelloAck records a hello_ack: a refusal sets the link to rejected, and an
// acceptance is checked for clock skew against the cloud's clock.
func (c *Collector) noteHelloAck(ack proto.HelloAck) {
	if !ack.Accepted {
		c.setLink(LinkRejected, string(ack.Reason))
		return
	}
	c.checkClockSkew(ack.ServerTime)
}

// checkClockSkew warns when this host's clock is more than clockSkewThreshold
// away from the cloud's (serverMS is the hello_ack's server_time). The frame's
// transit time is ignored: it is far below the threshold.
func (c *Collector) checkClockSkew(serverMS int64) {
	if serverMS <= 0 {
		return // an older cloud that does not send server_time
	}
	now := time.Now()
	skew := now.Sub(time.UnixMilli(serverMS))
	if skew > -clockSkewThreshold && skew < clockSkewThreshold {
		return
	}
	c.link.mu.Lock()
	throttled := !c.link.lastSkewWarn.IsZero() && now.Sub(c.link.lastSkewWarn) < clockSkewWarnEvery
	if !throttled {
		c.link.lastSkewWarn = now
	}
	c.link.mu.Unlock()
	if throttled {
		return
	}
	direction := "ahead of"
	if skew < 0 {
		direction = "behind"
	}
	c.log.Warn().
		Dur("skew", skew.Round(time.Second)).
		Msgf("cloud: this host's clock is %s %s DockTail Cloud's; event and check times will be off. Enable time sync (NTP) on the host", skew.Abs().Round(time.Second), direction)
}
