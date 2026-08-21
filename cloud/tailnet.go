package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/marvinvr/docktail/cloud/proto"
	"github.com/marvinvr/docktail/tailscale"
)

// tailnetSource is the read-only tailscale view the collector needs: the local
// daemon (peer liveness, this node's identity, the tailnet name) plus the
// control-plane reads that answer a [proto.TailnetProbe] with the credentials
// DockTail already holds. *tailscale.Client satisfies it. Nil when DockTail has
// no tailscale client, in which case the collector reports no peer liveness and
// answers every probe as unavailable.
type tailnetSource interface {
	// Status returns this node's stable ID, its tailnet name, and the liveness
	// of the tailnet peers it can see, from `tailscale status`.
	Status(ctx context.Context) (*tailscale.TailnetStatus, error)
	// APIEnabled reports whether Tailscale API credentials are configured.
	// False ⇒ the control plane is unreadable and no probe can be answered.
	APIEnabled() bool
	// ServiceControlState reads the control plane's view of one service.
	ServiceControlState(ctx context.Context, serviceName string) (tailscale.ServiceControlState, error)
}

// tailnetProbeTimeout bounds one whole control-plane read (status plus every
// service lookup) so a hanging Tailscale API cannot wedge the probe. Services
// still outstanding when it fires fail individually, which the cloud holds
// against their previous state rather than reading as an outage.
const tailnetProbeTimeout = 60 * time.Second

// handleTailnetProbe answers a cloud [proto.TailnetProbe] with a
// [proto.TailnetControlReport]. It runs off the read loop because the
// control-plane read is network I/O, and it is a metadata read only: the probe
// carries no destination and nothing to execute, just service names this host
// already published.
func (c *Collector) handleTailnetProbe(ctx context.Context, conn *wsConn, probe proto.TailnetProbe) {
	c.mu.RLock()
	unmonitored := c.unmonitored
	c.mu.RUnlock()
	if unmonitored {
		// The cloud drops operational frames from an unmonitored host, so
		// answering would spend the customer's Tailscale API quota for nothing.
		return
	}
	report := c.tailnetControlReport(ctx, probe)
	report.RequestID = probe.RequestID
	c.send(conn, proto.TypeTailnetControl, report)
	c.log.Debug().
		Str("request_id", probe.RequestID).
		Bool("available", report.Available).
		Str("reason", report.Reason).
		Int("services", len(report.Services)).
		Msg("cloud: tailnet control report sent")
}

// tailnetControlReport reads the Tailscale control plane for the probed
// services. Serialized: two overlapping probes must not double-spend the quota.
func (c *Collector) tailnetControlReport(ctx context.Context, probe proto.TailnetProbe) proto.TailnetControlReport {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	if c.tailnet == nil || !c.tailnet.APIEnabled() {
		// The expected case for most installs (no OAuth client or API key), not
		// a fault: the cloud says "tailnet health needs credentials" instead of
		// showing an outage, so this stays at debug.
		c.log.Debug().Msg("cloud: tailnet probe: no tailscale API credentials")
		return unavailableControlReport(proto.ControlUnavailNoCredentials, "")
	}

	// The AGENT, not the cloud, enforces the floor between two control-plane
	// reads: the quota being spent is the customer's, so a buggy or hostile
	// control plane must never be able to turn it into a rate-limit incident.
	// Asked again too soon we re-serve the previous answer verbatim (the caller
	// stamps the new RequestID). It covers the previous probe's service set,
	// which is acceptable because the cloud's own cadence is minutes.
	gap := time.Duration(proto.MinTailnetProbeIntervalMS) * time.Millisecond
	if !c.probedAt.IsZero() && time.Since(c.probedAt) < gap {
		c.log.Debug().Str("request_id", probe.RequestID).Msg("cloud: tailnet probe below minimum interval, serving cached report")
		return c.probeReport
	}

	cctx, cancel := context.WithTimeout(ctx, tailnetProbeTimeout)
	defer cancel()

	st, err := c.tailnet.Status(cctx)
	if err != nil || st == nil {
		c.log.Debug().Err(err).Msg("cloud: tailnet probe: no tailnet")
		return c.rememberControlReport(unavailableControlReport(proto.ControlUnavailNoTailnet, ""))
	}

	names, keys := dedupeProbeServices(probe.Services)
	report := proto.TailnetControlReport{
		Tailnet:   st.Tailnet,
		Available: true,
		CheckedAt: nowMillis(),
		Services:  make([]proto.TailnetControlState, 0, len(names)),
	}
	for i, name := range names {
		state, serr := c.tailnet.ServiceControlState(cctx, name)
		if serr != nil {
			// A failure on the FIRST lookup is global — rejected credentials, a
			// missing scope, quota, or a dead API — so the whole report is
			// unavailable with an operator-actionable reason. Once one lookup has
			// succeeded the credentials demonstrably work, so a later failure is
			// about that one service: it gets an Error and the cloud keeps its
			// previous state instead of inventing an outage.
			if i == 0 {
				reason := controlUnavailReason(serr)
				c.log.Warn().Err(serr).Str("reason", reason).Msg("cloud: tailnet control plane unreadable")
				return c.rememberControlReport(unavailableControlReport(reason, st.Tailnet))
			}
			c.log.Debug().Err(serr).Str("service", name).Msg("cloud: tailnet control read failed for service")
			report.Services = append(report.Services, erroredControlStates(name, keys[name], controlErrorText(serr))...)
			continue
		}
		hosts := make([]proto.TailnetControlHost, 0, len(state.Hosts))
		for _, h := range state.Hosts {
			hosts = append(hosts, proto.TailnetControlHost{
				NodeID:        h.NodeID,
				State:         controlStateToProto(h.State),
				ApprovalLevel: h.ApprovalLevel,
				Configured:    h.Configured,
			})
		}
		// One API call answers for every service key that shares the name.
		for _, key := range keys[name] {
			report.Services = append(report.Services, proto.TailnetControlState{
				ServiceKey:  key,
				ServiceName: state.ServiceName,
				Exists:      state.Exists,
				Hosts:       hosts,
			})
		}
	}
	return c.rememberControlReport(report)
}

// rememberControlReport caches the answer and starts the minimum-interval clock.
// Failures are cached too: a report that already cost an API call (or proved the
// credentials wrong) must not be retried on the cloud's cadence.
func (c *Collector) rememberControlReport(report proto.TailnetControlReport) proto.TailnetControlReport {
	c.probedAt = time.Now()
	c.probeReport = report
	return report
}

// unavailableControlReport is an "I could not answer" report. Available=false
// with a ControlUnavail* reason is explicitly not an outage: the cloud shows the
// operator what to fix instead of a phantom failure.
func unavailableControlReport(reason, tailnet string) proto.TailnetControlReport {
	return proto.TailnetControlReport{
		Tailnet:   tailnet,
		Available: false,
		Reason:    reason,
		CheckedAt: nowMillis(),
	}
}

// erroredControlStates marks every key behind one service name as unread.
func erroredControlStates(serviceName string, keys []string, msg string) []proto.TailnetControlState {
	out := make([]proto.TailnetControlState, 0, len(keys))
	for _, key := range keys {
		out = append(out, proto.TailnetControlState{ServiceKey: key, ServiceName: serviceName, Error: msg})
	}
	return out
}

// dedupeProbeServices reduces the probe to the unique tailscale service names to
// query, in request order, plus the service keys each name answers for. Several
// keys can share one service name (the same service on several ports) and each
// NAME costs exactly one API call, so the customer's quota scales with the
// tailnet's services rather than with the cloud's catalog. Entries past
// [proto.MaxTailnetProbeServices] and entries missing a key or a name are ignored.
func dedupeProbeServices(in []proto.TailnetProbeService) (names []string, keys map[string][]string) {
	if len(in) > proto.MaxTailnetProbeServices {
		in = in[:proto.MaxTailnetProbeServices]
	}
	keys = make(map[string][]string, len(in))
	queriedFor := make(map[string]string, len(in)) // fold identity -> name actually queried
	for _, svc := range in {
		key := strings.TrimSpace(svc.ServiceKey)
		name := strings.TrimSpace(svc.ServiceName)
		if key == "" || name == "" {
			continue
		}
		// "myapp" and "svc:myapp" address the same service; fold them so they
		// don't cost two calls.
		identity := strings.ToLower(strings.TrimPrefix(name, "svc:"))
		queried, seen := queriedFor[identity]
		if !seen {
			queried = name
			queriedFor[identity] = name
			names = append(names, name)
		}
		keys[queried] = append(keys[queried], key)
	}
	return names, keys
}

// controlStateToProto maps the tailscale package's normalized state onto the
// wire vocabulary. Explicit rather than a pass-through so the two can drift
// without silently changing the wire; anything unrecognized becomes unknown,
// never a failure state.
func controlStateToProto(state string) string {
	switch state {
	case tailscale.ControlStateConnected:
		return proto.ControlStateConnected
	case tailscale.ControlStatePendingApproval:
		return proto.ControlStatePendingApproval
	case tailscale.ControlStateNeedsConfig:
		return proto.ControlStateNeedsConfig
	case tailscale.ControlStatePreApproved:
		return proto.ControlStatePreApproved
	case tailscale.ControlStateDraining:
		return proto.ControlStateDraining
	}
	return proto.ControlStateUnknown
}

// controlUnavailReason maps a failed control-plane read onto the reason the UI
// shows. The distinctions are the actionable ones: rotate the credential (401),
// grant the Services read scope (403), or wait (429).
func controlUnavailReason(err error) string {
	switch tailscale.APIStatusCode(err) {
	case http.StatusUnauthorized:
		return proto.ControlUnavailUnauthorized
	case http.StatusForbidden:
		return proto.ControlUnavailForbidden
	case http.StatusTooManyRequests:
		return proto.ControlUnavailRateLimited
	}
	return proto.ControlUnavailAPIError
}

// controlErrorText summarizes a per-service read failure. The API response body
// is deliberately dropped: it is unbounded third-party text and this string
// leaves the host.
func controlErrorText(err error) string {
	if code := tailscale.APIStatusCode(err); code != 0 {
		return fmt.Sprintf("tailscale api: http %d", code)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "tailscale api: timed out"
	}
	return "tailscale api: request failed"
}

// tailnetIdentity reads this node's tailscale StableNodeID and tailnet name
// (best-effort, bounded) for the hello frame, in ONE `tailscale status` call.
// The node ID is how the cloud splits THIS host's outages into agent_down vs
// host_down (empty ⇒ it falls back to host_down) and how it matches this host
// against a service's control-plane hosts; the tailnet name groups hosts that
// share a control plane, so the cloud can probe one of them on behalf of all.
func (c *Collector) tailnetIdentity(ctx context.Context) (nodeID, tailnet string) {
	if c.tailnet == nil {
		return "", ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := c.tailnet.Status(cctx)
	if err != nil || st == nil {
		return "", ""
	}
	return st.SelfNodeID, st.Tailnet
}

// tailnetLoop reports the host's local-netmap peer liveness on the heartbeat
// cadence. It is the signal the cloud uses to tell a dead agent (the device is
// still online) from a dead host (device gone) for OTHER hosts on the tailnet.
// Skipped while unmonitored (the cloud drops the frame) and silent when there is
// no tailnet.
func (c *Collector) tailnetLoop(ctx context.Context, conn *wsConn) {
	ticker := time.NewTicker(proto.HeartbeatInterval * time.Second)
	defer ticker.Stop()
	c.sampleAndSendTailnet(ctx, conn)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sampleAndSendTailnet(ctx, conn)
		}
	}
}

func (c *Collector) sampleAndSendTailnet(ctx context.Context, conn *wsConn) {
	if c.tailnet == nil {
		return
	}
	c.mu.RLock()
	unmonitored := c.unmonitored
	c.mu.RUnlock()
	if unmonitored {
		return
	}
	st, err := c.tailnet.Status(ctx)
	if err != nil || st == nil {
		return // no tailnet → nothing to report
	}
	peers := make([]proto.TailnetPeer, 0, len(st.Peers))
	for _, p := range st.Peers {
		if p.NodeID == "" {
			continue
		}
		tp := proto.TailnetPeer{
			NodeID:   p.NodeID,
			Hostname: p.Hostname,
			Online:   p.Online,
		}
		if !p.Online && !p.LastSeen.IsZero() {
			tp.LastSeen = p.LastSeen.UnixMilli()
		}
		peers = append(peers, tp)
	}
	if len(peers) == 0 {
		return
	}
	c.send(conn, proto.TypeTailnet, proto.TailnetReport{Peers: peers})
}
