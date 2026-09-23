// Package proto defines the wire contract between the DockTail agent (OSS,
// AGPL, github.com/marvinvr/docktail) and the DockTail Cloud agent-plane
// (proprietary).
//
// It is intentionally dependency-free (stdlib only) so it stays a clean
// licensing firewall: the AGPL agent and the proprietary cloud both import an
// identical, neutral set of types without either contaminating the other. The
// intended end-state is a standalone Apache-2.0 module
// (github.com/marvinvr/docktail-proto) that both sides import.
//
// NOTE: until that module exists, this package lives as two hand-synced copies
// — proto/ in the DockTail Cloud repo (the source of truth) and cloud/proto in
// the agent repo — that MUST stay byte-identical. Every change lands in both,
// and every field added to the wire is omitempty and backward-compatible. The
// cloud's CI diffs its copy against a pinned agent commit.
//
// Transport: outbound-only WSS, JSON messages, 30s heartbeat (doubles as
// liveness), jittered reconnect. Every frame is an [Envelope] carrying a
// typed payload. The protocol is metadata-only: there are no exec, deploy,
// or shell message types, on purpose — the non-goals are enforced
// structurally and verifiable in the open agent source.
package proto
