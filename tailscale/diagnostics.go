package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"
)

// A node hosts a Tailscale Service only when two independent pieces of local
// state agree:
//
//  1. the serve config carries handlers for it (`tailscale serve --service=...`)
//  2. prefs.AdvertiseServices lists it (cleared by `serve drain` and `serve clear`)
//
// tailscaled merges the two into []tailcfg.VIPService{Name, Ports, Active},
// hashes that list, and sends only the hash to the control plane in
// Hostinfo.ServicesHash; control then pulls the real list back over c2n. The
// client re-pushes only when the hash changes.
//
// DockTail's reconciler reads `tailscale serve status`, which shows (1) and not
// (2). Everything in this file exists to make (2) — and the hash-relevant
// merge of both — observable, because a service that is configured but not
// advertised looks perfectly healthy to every command DockTail otherwise runs.

// AdvertisedServices returns the services this node currently advertises, as
// recorded in prefs.AdvertiseServices.
func (c *Client) AdvertisedServices(ctx context.Context) ([]string, error) {
	cmd := c.tailscaleCmd(ctx, "debug", "prefs")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to read tailscale prefs: %w", err)
	}

	var prefs struct {
		AdvertiseServices []string `json:"AdvertiseServices"`
	}
	if err := json.Unmarshal([]byte(stripWarnings(output)), &prefs); err != nil {
		return nil, fmt.Errorf("failed to parse tailscale prefs: %w", err)
	}

	sort.Strings(prefs.AdvertiseServices)
	return prefs.AdvertiseServices, nil
}

// DaemonState is the tailscaled-side context worth recording alongside the
// service state: which node this is, whether the backend is up, and any health
// warnings the daemon is reporting.
type DaemonState struct {
	BackendState string   `json:"backend_state"`
	NodeID       string   `json:"node_id"`
	Health       []string `json:"health,omitempty"`
}

// DaemonState reads tailscaled's backend state, this node's stable ID and its
// current health warnings. Health warnings are the daemon's own account of what
// is wrong and are not surfaced anywhere else in DockTail.
func (c *Client) DaemonState(ctx context.Context) (*DaemonState, error) {
	cmd := c.tailscaleCmd(ctx, "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to read tailscale status: %w", err)
	}

	var st struct {
		BackendState string   `json:"BackendState"`
		Health       []string `json:"Health"`
		Self         *struct {
			ID string `json:"ID"`
		} `json:"Self"`
	}
	if err := json.Unmarshal([]byte(stripWarnings(output)), &st); err != nil {
		return nil, fmt.Errorf("failed to parse tailscale status: %w", err)
	}

	out := &DaemonState{BackendState: st.BackendState, Health: st.Health}
	if st.Self != nil {
		out.NodeID = st.Self.ID
	}
	return out, nil
}

// ServeConfigSnapshot returns the DockTail-managed services present in the
// serve config. It is the same data GetCurrentServices returns, without the
// per-call Info logging, so a diagnostics loop can poll it frequently without
// drowning the normal log.
func (c *Client) ServeConfigSnapshot(ctx context.Context) (map[string]ServiceEndpoint, error) {
	cmd := c.tailscaleCmd(ctx, "serve", "status", "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		if isNotFoundError(stderr) {
			return make(map[string]ServiceEndpoint), nil
		}
		return nil, fmt.Errorf("failed to read serve status: %w (output: %s)", err, stderr)
	}

	var status TailscaleStatus
	if err := json.Unmarshal([]byte(stripWarnings(output)), &status); err != nil {
		return nil, fmt.Errorf("failed to parse serve status: %w", err)
	}

	log.Debug().
		Int("services", len(status.Services)).
		Msg("Diagnostics read serve config")

	return parseManagedServices(status), nil
}

// ServiceState is one service's full hosting state: the serve-config half, the
// advertisement half, and whether the two agree.
type ServiceState struct {
	Name        string   `json:"name"`
	Ports       []string `json:"ports"`
	Destination string   `json:"destination,omitempty"`
	Protocol    string   `json:"protocol,omitempty"`
	Configured  bool     `json:"configured"`
	Advertised  bool     `json:"advertised"`
}

// Active reports whether tailscaled will report this service to control as one
// this node actively hosts (tailcfg.VIPService.Active).
func (s ServiceState) Active() bool { return s.Advertised }

// Anomaly describes a disagreement between the two halves, or the empty string
// when the service is in a coherent state.
func (s ServiceState) Anomaly() string {
	switch {
	case s.Configured && !s.Advertised:
		return "configured but not advertised (the node serves it locally, but control is not told to route to it)"
	case s.Advertised && !s.Configured:
		return "advertised but has no serve config (control may route to this node while it has nothing to serve)"
	case s.Advertised && s.Configured && len(s.Ports) == 0:
		return "advertised with no ports"
	}
	return ""
}

// ServiceStates merges the serve config and the advertisement list into the
// per-service view, covering services that appear in only one of the two.
func ServiceStates(serveConfig map[string]ServiceEndpoint, advertised []string) []ServiceState {
	advertisedSet := make(map[string]struct{}, len(advertised))
	for _, name := range advertised {
		advertisedSet[name] = struct{}{}
	}

	byName := make(map[string]*ServiceState)
	for _, endpoint := range serveConfig {
		state, ok := byName[endpoint.ServiceName]
		if !ok {
			state = &ServiceState{Name: endpoint.ServiceName, Configured: true}
			byName[endpoint.ServiceName] = state
		}
		state.Ports = append(state.Ports, endpoint.Port)
		// A service can carry several ports; the destination and protocol of any
		// one of them is enough to spot a backend that moved.
		if state.Destination == "" {
			state.Destination = endpoint.Destination
			state.Protocol = endpoint.Protocol
		}
	}

	for name := range advertisedSet {
		if _, ok := byName[name]; !ok {
			byName[name] = &ServiceState{Name: name}
		}
	}

	states := make([]ServiceState, 0, len(byName))
	for name, state := range byName {
		_, state.Advertised = advertisedSet[name]
		sort.Strings(state.Ports)
		states = append(states, *state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Name < states[j].Name })
	return states
}

// VIPFingerprint renders the exact tuple tailscaled hashes into
// Hostinfo.ServicesHash: service name, ports, and whether it is active. The
// backend destination is deliberately absent because it is absent from the hash
// too — re-pointing a service at a new container IP never reaches control.
//
// When this string changes, control is notified and pulls the new list. When it
// does not change, control is never told anything, so comparing fingerprints
// across a container update shows whether control had any reason to look.
func VIPFingerprint(states []ServiceState) string {
	parts := make([]string, 0, len(states))
	for _, s := range states {
		active := "inactive"
		if s.Active() {
			active = "active"
		}
		parts = append(parts, fmt.Sprintf("%s[%s]=%s", s.Name, strings.Join(s.Ports, ","), active))
	}
	return strings.Join(parts, ";")
}
