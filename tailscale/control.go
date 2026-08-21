package tailscale

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Control-plane service states, normalized from the Tailscale API's raw
// approvalLevel/configured strings by normalizeControlState. They mirror the
// states the Tailscale admin console shows for a Service's hosts.
//
// This vocabulary is deliberately duplicated rather than imported from
// cloud/proto: the tailscale package must not depend on the cloud wire types
// (the cloud module translates these across the boundary). The values match the
// proto constants so the translation stays a straight mapping.
const (
	ControlStateConnected       = "connected"           // advertised and approved — reachable
	ControlStatePendingApproval = "pending_approval"    // advertised, awaiting admin approval — NOT reachable
	ControlStateNeedsConfig     = "needs_configuration" // host known, config invalid or missing
	ControlStatePreApproved     = "pre_approved"        // auto-approved but not yet advertising
	ControlStateDraining        = "draining"            // deliberately winding down; existing conns still served
	ControlStateUnknown         = "unknown"             // vocabulary we don't recognize
)

// APIStatusError is a non-2xx response from the Tailscale API. It carries the
// status code so callers can act on it (404 = no such Service definition, 401 =
// credentials rejected, 403 = missing scope, 429 = quota) instead of matching on
// error strings.
type APIStatusError struct {
	Op         string // short description of the call, e.g. "list service hosts"
	StatusCode int
	Body       string
}

func (e *APIStatusError) Error() string {
	return fmt.Sprintf("%s API returned error status %d: %s", e.Op, e.StatusCode, e.Body)
}

// APIStatusCode returns the HTTP status carried by err, or 0 when err is not an
// *APIStatusError (transport failure, decode failure, context cancellation).
func APIStatusCode(err error) int {
	var se *APIStatusError
	if errors.As(err, &se) {
		return se.StatusCode
	}
	return 0
}

// ServiceControlState is the Tailscale *control plane's* view of one Service:
// whether the tailnet has a definition for it at all, and which devices it
// believes are advertising it. It is an approval/advertisement oracle, not a
// reachability probe — Tailscale exposes no liveness check for a Service.
//
// Exists is false when the tailnet has no such Service definition (HTTP 404):
// a host may still be serving it locally, but nothing can resolve it.
type ServiceControlState struct {
	ServiceName string // as queried, "svc:"-prefixed
	Exists      bool
	Hosts       []ServiceControlHost
}

// ServiceControlHost is one device advertising a Service. State is the
// normalized ControlState*; ApprovalLevel and Configured are the raw API strings,
// kept for display and so an unrecognized vocabulary is still visible to a human.
type ServiceControlHost struct {
	NodeID        string // tailscale StableNodeID
	State         string // ControlState*
	ApprovalLevel string // raw
	Configured    string // raw
}

// APIEnabled reports whether Tailscale API credentials (OAuth client or API key)
// are configured. Without them only the local daemon is readable, so every
// control-plane read is unavailable rather than failing.
func (c *Client) APIEnabled() bool { return c.apiSyncEnabled }

// ServiceControlState reads the control plane's view of one Service.
//
// A 404 is not an error: it is the answer "this tailnet has no such Service
// definition", which is exactly the failure mode the local serve config cannot
// see. Every other non-2xx is returned as an *APIStatusError so the caller can
// map 401/403/429 onto the right operator-actionable reason.
func (c *Client) ServiceControlState(ctx context.Context, serviceName string) (ServiceControlState, error) {
	name := strings.TrimSpace(serviceName)
	if name == "" {
		return ServiceControlState{}, fmt.Errorf("empty service name")
	}
	// The API addresses Services by their "svc:"-prefixed name; labels may carry
	// either form (same normalization as SyncServiceDefinition).
	if !strings.HasPrefix(name, "svc:") {
		name = "svc:" + name
	}

	out := ServiceControlState{ServiceName: name}
	hosts, err := c.listServiceHosts(ctx, name)
	if err != nil {
		if APIStatusCode(err) == http.StatusNotFound {
			return out, nil // Exists=false
		}
		return ServiceControlState{}, err
	}
	out.Exists = true
	for _, h := range hosts {
		// The Services API calls this field nodeId. It is the same stable
		// n…CNTRL identity that `tailscale status --json` exposes as Self.ID,
		// which lets Cloud match the advertisement to the reporting host.
		if h.NodeID == "" {
			continue
		}
		out.Hosts = append(out.Hosts, ServiceControlHost{
			NodeID:        h.NodeID,
			State:         normalizeControlState(h.ApprovalLevel, h.Configured),
			ApprovalLevel: h.ApprovalLevel,
			Configured:    h.Configured,
		})
	}
	return out, nil
}

// normalizeControlState maps the raw approvalLevel/configured pair onto the
// ControlState* vocabulary.
//
// The raw strings are UNVERIFIED against a live tailnet — this is the single
// place they are interpreted, so matching is case-insensitive and by substring,
// and anything unrecognized falls through to ControlStateUnknown. An unfamiliar
// value from a newer control plane must never be read as a failure: a monitoring
// agent that invents an outage from a vocabulary change is worse than one that
// admits it doesn't know. The raw strings travel alongside the normalized state
// so a human can still see what the API actually said.
//
// Order matters: the more specific states are tested first ("pre-approved"
// contains "approved"; a host that is approved but misconfigured is not
// reachable, so the configuration verdict outranks the approval one).
func normalizeControlState(approvalLevel, configured string) string {
	approval := strings.ToLower(strings.TrimSpace(approvalLevel))
	config := strings.ToLower(strings.TrimSpace(configured))
	both := approval + " " + config

	switch {
	case strings.Contains(both, "drain"):
		return ControlStateDraining
	case containsAny(approval, "unapprov", "not approv", "denied", "reject"):
		// Undocumented negations: refuse to read them as "approved" (the
		// substring would match) and refuse to guess which failure they are.
		return ControlStateUnknown
	case containsAny(both, "pending", "awaiting"):
		return ControlStatePendingApproval
	case containsAny(approval, "pre-approved", "pre_approved", "preapproved"):
		return ControlStatePreApproved
	case containsAny(config, "needs", "unconfigured", "not configured", "not-configured", "missing", "invalid"):
		return ControlStateNeedsConfig
	case containsAny(approval, "approved", "connected"):
		return ControlStateConnected
	}
	return ControlStateUnknown
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
