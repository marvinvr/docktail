package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// TailnetStatus is a minimal view of `tailscale status --json`: this node's
// stable ID, the tailnet it belongs to, and the online/offline status of the
// tailnet devices (peers) it can see. DockTail Cloud uses it (read over the
// local daemon, no API key) to report peer device liveness so the cloud can tell
// a dead agent from a dead host.
type TailnetStatus struct {
	SelfNodeID string
	// Tailnet names the tailnet this node is currently in. Best-effort: older
	// daemons and a logged-out node report nothing, so an empty value means
	// "unknown", never "no tailnet".
	Tailnet string
	Peers   []TailnetPeerStatus
}

// TailnetPeerStatus is one tailnet device's liveness as seen in this node's netmap.
type TailnetPeerStatus struct {
	NodeID   string
	Hostname string
	Online   bool
	LastSeen time.Time
}

// statusJSON is the subset of `tailscale status --json` (tailscaled's
// ipnstate.Status) that we parse.
type statusJSON struct {
	Self           *statusNode            `json:"Self"`
	Peer           map[string]*statusNode `json:"Peer"`
	CurrentTailnet *statusTailnet         `json:"CurrentTailnet"`
}

// statusTailnet is ipnstate.Status.CurrentTailnet. Name is the human-facing
// tailnet name; MagicDNSSuffix (e.g. "tail1234.ts.net") is the fallback because
// it identifies the same tailnet and is present whenever MagicDNS is on.
type statusTailnet struct {
	Name           string `json:"Name"`
	MagicDNSSuffix string `json:"MagicDNSSuffix"`
}

type statusNode struct {
	ID       string    `json:"ID"`
	HostName string    `json:"HostName"`
	Online   bool      `json:"Online"`
	LastSeen time.Time `json:"LastSeen"`
}

// Status runs `tailscale status --json` and parses this node's stable ID, its
// tailnet name, and the liveness of the peers it can see. Returns an error if
// the daemon isn't reachable or the output can't be parsed — callers treat that
// as "no tailnet" and skip reporting.
func (c *Client) Status(ctx context.Context) (*TailnetStatus, error) {
	cmd := c.tailscaleCmd(ctx, "status", "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w (output: %s)", err, string(output))
	}
	var st statusJSON
	if err := json.Unmarshal([]byte(stripWarnings(output)), &st); err != nil {
		return nil, fmt.Errorf("tailscale status: parse json: %w", err)
	}
	out := &TailnetStatus{}
	if st.Self != nil {
		out.SelfNodeID = st.Self.ID
	}
	if st.CurrentTailnet != nil {
		out.Tailnet = st.CurrentTailnet.Name
		if out.Tailnet == "" {
			out.Tailnet = st.CurrentTailnet.MagicDNSSuffix
		}
	}
	for _, p := range st.Peer {
		if p == nil || p.ID == "" {
			continue
		}
		out.Peers = append(out.Peers, TailnetPeerStatus{
			NodeID:   p.ID,
			Hostname: p.HostName,
			Online:   p.Online,
			LastSeen: p.LastSeen,
		})
	}
	return out, nil
}
