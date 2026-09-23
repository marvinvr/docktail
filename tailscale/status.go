package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TailnetStatus is a minimal view of `tailscale status --json`: this node's
// stable ID and MagicDNS name, and the tailnet it belongs to. DockTail Cloud
// reads it over the local daemon (no API key) to identify this node and to say
// where its Funnel exposure answers on the public internet.
type TailnetStatus struct {
	SelfNodeID string
	// SelfDNSName is this node's MagicDNS name with the trailing dot stripped
	// (e.g. "box.tail1234.ts.net"). It is the host part of every Funnel URL this
	// node serves. Empty when the daemon reports none — not logged in, or MagicDNS
	// disabled — in which case there is no public address to report.
	SelfDNSName string
	// Tailnet names the tailnet this node is currently in. Best-effort: older
	// daemons and a logged-out node report nothing, so an empty value means
	// "unknown", never "no tailnet".
	Tailnet string
}

// statusJSON is the subset of `tailscale status --json` (tailscaled's
// ipnstate.Status) that we parse.
type statusJSON struct {
	Self           *statusNode    `json:"Self"`
	CurrentTailnet *statusTailnet `json:"CurrentTailnet"`
}

// statusTailnet is ipnstate.Status.CurrentTailnet. Name is the human-facing
// tailnet name; MagicDNSSuffix (e.g. "tail1234.ts.net") is the fallback because
// it identifies the same tailnet and is present whenever MagicDNS is on.
type statusTailnet struct {
	Name           string `json:"Name"`
	MagicDNSSuffix string `json:"MagicDNSSuffix"`
}

type statusNode struct {
	ID      string `json:"ID"`
	DNSName string `json:"DNSName"` // FQDN with a trailing dot, e.g. "box.tail1234.ts.net."
}

// Status runs `tailscale status --json` and parses this node's stable ID,
// MagicDNS name and tailnet name. Returns an error if
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
		out.SelfDNSName = strings.TrimSuffix(strings.TrimSpace(st.Self.DNSName), ".")
	}
	if st.CurrentTailnet != nil {
		out.Tailnet = st.CurrentTailnet.Name
		if out.Tailnet == "" {
			out.Tailnet = st.CurrentTailnet.MagicDNSSuffix
		}
	}
	return out, nil
}
