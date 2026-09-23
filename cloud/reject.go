package cloud

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/marvinvr/docktail/cloud/proto"
)

// dashboardURL is the DockTail Cloud dashboard that rejection hints point at.
// A DOCKTAIL_CLOUD_URL override (local development) does not change it.
const dashboardURL = "https://cloud.docktail.org"

const (
	// rareRetryInterval is how often the collector asks again after a rejection
	// only an operator (or a fixed cloud) can lift — a blocked host, a refused
	// protocol version, a 403 from something in front of the cloud. None needs the
	// agent restarted to clear, so it keeps knocking, but rarely.
	rareRetryInterval = 15 * time.Minute
	// rareRetryWindow bounds that knocking: after this long the collector stops
	// like any terminal rejection, and a restart resumes it.
	rareRetryWindow = 24 * time.Hour
	// reminderInterval is how often a stopped collector repeats why, and how often
	// a retrying one repeats the full hint (the retries in between log one short
	// line), so the cause stays near the tail of the container log.
	reminderInterval = 30 * time.Minute
)

// rejectAction is what the collector does after the cloud refuses a connection.
type rejectAction int

const (
	rejectRetry     rejectAction = iota // ordinary exponential backoff
	rejectRetrySlow                     // operator-actionable: retry every 30–60 s
	rejectRetryRare                     // retry about every rareRetryInterval, for at most rareRetryWindow
	rejectStop                          // nothing changes until the container restarts
)

// rejection is a refused connection translated for the operator: the machine
// reason (a RejectCode, or "http_<status>" for a refused upgrade), what the
// collector does next, and one or two sentences saying what happened and how to
// fix it. A rejectRetryRare rejection also carries stopHint, the hint for when
// rareRetryWindow runs out.
type rejection struct {
	reason   string
	action   rejectAction
	hint     string
	stopHint string
}

var (
	agentKeysURL = dashboardURL + "/settings/agent-keys"
	hostsURL     = dashboardURL + "/hosts"
	billingURL   = dashboardURL + "/settings/billing"
)

// invalidKeyHint covers every "this key no longer authenticates" outcome. The
// key comes from the environment, so any fix needs a restart.
var invalidKeyHint = fmt.Sprintf("DockTail Cloud does not accept this workspace key: it was revoked, its workspace was deleted, or it is mistyped. "+
	"Create a new agent key at %s, set it as %s, then recreate this container.", agentKeysURL, EnvKey)

// httpRejection classifies a refused WSS upgrade. Only 401/403 are rejections;
// any other status is an ordinary retryable dial failure (nil). DockTail Cloud
// answers an unknown or revoked key with 401; it has no 403 for an agent, so a
// 403 comes from something in between (a proxy, firewall or WAF) and is not the
// key's fault.
func httpRejection(status int) *rejection {
	switch status {
	case http.StatusUnauthorized:
		return &rejection{reason: "http_401", action: rejectStop, hint: invalidKeyHint}
	case http.StatusForbidden:
		lead := "Something between this host and DockTail Cloud (a proxy, firewall or WAF) refused the connection with HTTP 403. " +
			"Allow outbound WebSocket connections to the DockTail Cloud endpoint"
		return &rejection{
			reason:   "http_403",
			action:   rejectRetryRare,
			hint:     lead + "; " + rareRetryNote + ".",
			stopHint: lead + ", then restart this container.",
		}
	default:
		return nil
	}
}

// rareRetryNote tells the operator how a rejectRetryRare rejection proceeds.
var rareRetryNote = fmt.Sprintf("the agent checks again about every %d minutes for up to %s, and after that needs a restart",
	int(rareRetryInterval/time.Minute), formatHours(rareRetryWindow))

// helloRejection classifies a non-accepted hello_ack.
func helloRejection(code proto.RejectCode) rejection {
	r := rejection{reason: string(code)}
	switch code {
	case proto.RejectInvalidKey:
		r.action, r.hint = rejectStop, invalidKeyHint
	case proto.RejectBlocked:
		lead := fmt.Sprintf("This host is blocked in DockTail Cloud. Unblock it at %s", hostsURL)
		r.action = rejectRetryRare
		r.hint = lead + "; " + rareRetryNote + "."
		r.stopHint = lead + ", then restart this container."
	case proto.RejectProtocolMismatch:
		// Stopping would be right for an outdated image, but a cloud-side mistake
		// would then silence every agent until each is restarted; a rare retry
		// heals that on its own.
		lead := fmt.Sprintf("DockTail Cloud does not accept this agent's wire protocol (DockTail %s, protocol v%d). "+
			"Pull the latest DockTail image and recreate this container", agentVersion, proto.ProtocolVersion)
		r.action = rejectRetryRare
		r.hint = lead + "; until then " + rareRetryNote + "."
		r.stopHint = lead + "."
	case proto.RejectEnrollmentClosed:
		r.action = rejectRetrySlow
		r.hint = fmt.Sprintf("This workspace key's enrollment window has closed, so it cannot add a new host. "+
			"Reopen enrollment for the key at %s (or create a new key and recreate this container with it); the agent retries automatically.", agentKeysURL)
	case proto.RejectOverCap:
		r.action = rejectRetrySlow
		r.hint = fmt.Sprintf("This workspace has reached its host limit. Upgrade the plan at %s or remove an offline host at %s; the agent retries automatically.",
			billingURL, hostsURL)
	default:
		// duplicate_identity (legacy), an empty reason, or a code newer than this
		// agent: none is known to be permanent, so keep the ordinary backoff.
		r.action = rejectRetry
		r.hint = fmt.Sprintf("DockTail Cloud rejected the connection (reason %q); the agent retries automatically.", code)
	}
	return r
}

// gaveUp turns a rejectRetryRare rejection into the terminal one it ends in
// once rareRetryWindow has passed.
func (r rejection) gaveUp() rejection {
	return rejection{reason: r.reason, action: rejectStop, hint: r.stopHint}
}

func formatHours(d time.Duration) string {
	return fmt.Sprintf("%d hours", int(d/time.Hour))
}

// stopped logs a terminal rejection and then repeats it every
// reminderInterval until ctx ends, so an operator reading the latest
// container logs still finds the reason the host went quiet.
func (c *Collector) stopped(ctx context.Context, r rejection) {
	c.log.Error().Str("reason", r.reason).Msg("cloud: stopped reporting until this container restarts. " + r.hint)
	t := time.NewTicker(reminderInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.log.Error().Str("reason", r.reason).Msg("cloud: still not reporting. " + r.hint)
		}
	}
}
