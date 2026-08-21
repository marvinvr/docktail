package tailscale

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// tailscaleCmd creates an exec.Cmd for the tailscale CLI with the correct
// environment. When a version mismatch between the bundled CLI and the host's
// tailscaled has been detected, it sets TS_DEBUG_FAKE_IPC_VERSION so the CLI
// doesn't reject the connection.
func (c *Client) tailscaleCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	if sv := c.getServerVersion(); sv != "" {
		cmd.Env = append(os.Environ(), "TS_DEBUG_FAKE_IPC_VERSION="+sv)
	}
	return cmd
}

// versionMismatchRe matches the tailscale CLI warning about version mismatch
// and captures the server version string.
var versionMismatchRe = regexp.MustCompile(`tailscaled server version "([^"]+)"`)

// DetectVersionMismatch runs `tailscale version` and checks if the bundled CLI
// version differs from the tailscaled server version (common in "Tailscale on
// Host" setups where the socket is mounted from the host). If a mismatch is
// found, the server version is stored so that subsequent CLI calls use
// TS_DEBUG_FAKE_IPC_VERSION to bypass the check.
func (c *Client) DetectVersionMismatch(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "tailscale", "version")
	output, _ := cmd.CombinedOutput()
	outStr := string(output)

	if !strings.Contains(outStr, "!= tailscaled server version") {
		// Clear stale override so normal matched-version setups use default behavior.
		if prev := c.getServerVersion(); prev != "" {
			log.Info().
				Str("previous_server_version", prev).
				Msg("Tailscale CLI/daemon versions now aligned; disabling TS_DEBUG_FAKE_IPC_VERSION override")
			c.setServerVersion("")
		}
		return
	}

	matches := versionMismatchRe.FindStringSubmatch(outStr)
	if len(matches) < 2 {
		c.setServerVersion("")
		log.Warn().
			Str("output", outStr).
			Msg("Detected tailscale version mismatch but could not parse server version")
		return
	}

	c.setServerVersion(matches[1])
	log.Info().
		Str("server_version", matches[1]).
		Msg("Tailscale CLI/daemon version mismatch detected; will use TS_DEBUG_FAKE_IPC_VERSION for CLI calls")
}

// WarnIfSocketMissing logs a setup hint when the tailscaled socket does not
// exist. This is the usual failure mode on macOS and Windows Docker hosts: the
// host's Tailscale app exposes no Unix socket, and Docker Desktop cannot mount
// host sockets into its VM, so the containerized sidecar setup is required.
func (c *Client) WarnIfSocketMissing() {
	if c.socketPath == "" {
		return
	}
	if _, err := os.Stat(c.socketPath); err == nil {
		return
	}
	log.Warn().
		Str("socket", c.socketPath).
		Msg("Tailscale socket not found; if your Docker host is macOS or Windows, the host's Tailscale daemon cannot be shared with containers - use the sidecar setup: https://docktail.org/docs/#tailscale-sidecar")
}

// stripWarnings removes warning messages from Tailscale CLI output
// Warnings appear before the JSON and need to be stripped for parsing
func stripWarnings(output []byte) string {
	outputStr := string(output)
	jsonStart := strings.Index(outputStr, "{")
	if jsonStart > 0 {
		outputStr = outputStr[jsonStart:]
		log.Debug().
			Int("stripped_bytes", jsonStart).
			Msg("Stripped warning message from tailscale output")
	}
	return outputStr
}

// isNotFoundError checks if an error message indicates a resource doesn't exist
func isNotFoundError(stderr string) bool {
	return strings.Contains(stderr, "not found") ||
		strings.Contains(stderr, "does not exist") ||
		strings.Contains(stderr, "no services") ||
		strings.Contains(stderr, "nothing to show") ||
		strings.Contains(stderr, "no funnel")
}

// isConfigConflictError checks if an error is due to a configuration conflict
func isConfigConflictError(stderr string) bool {
	return strings.Contains(stderr, "already serving") ||
		strings.Contains(stderr, "want to serve") ||
		strings.Contains(stderr, "port is already serving")
}

// isUntaggedNodeError checks if the error is because the Tailscale node is not tagged
func isUntaggedNodeError(stderr string) bool {
	return strings.Contains(stderr, "service hosts must be tagged nodes")
}

// isServeConfigDeniedError reports whether tailscaled rejected a serve or
// Funnel write because the connecting process is neither root nor the
// configured operator. Common with rootless Docker against a host tailscaled.
func isServeConfigDeniedError(stderr string) bool {
	return strings.Contains(stderr, "serve config denied")
}

// serveConfigDeniedMessage is the operator-facing hint for isServeConfigDeniedError.
func serveConfigDeniedMessage(action string) string {
	return fmt.Sprintf("failed to %s: Tailscale denied the serve config write. "+
		"The process talking to tailscaled is not root and is not the configured operator. "+
		"This is common with rootless Docker on the host Tailscale setup.\n"+
		"To fix this:\n"+
		"  1. Host install: sudo tailscale set --operator=$USER\n"+
		"     The node still needs an ACL tag (sudo tailscale up --advertise-tags=tag:server --reset if it is not tagged yet).\n"+
		"  2. Or use the sidecar setup so DockTail talks to a containerized tailscaled instead of the host daemon.\n"+
		"Rootless Docker notes: https://docktail.org/docs/#rootless-docker", action)
}

// isManagedService checks if a service name has the "svc:" prefix
// This indicates it's managed by DockTail and safe to modify
func isManagedService(serviceName string) bool {
	return strings.HasPrefix(serviceName, "svc:")
}

func normalizeServiceName(serviceName string) string {
	normalized := strings.ToLower(strings.TrimSpace(serviceName))
	return strings.TrimPrefix(normalized, "svc:")
}

func (c *Client) shouldIgnoreService(serviceName string) bool {
	if len(c.ignoredServices) == 0 {
		return false
	}

	_, ok := c.ignoredServices[normalizeServiceName(serviceName)]
	return ok
}

// sameDestination reports whether two service destinations point to the same
// backend. Tailscale records plain-TCP forwards as "host:port" (without a
// scheme) in `serve status`, whereas HTTP/HTTPS handlers and buildDestination
// include a "proto://" prefix. The service protocol is compared separately
// during reconciliation, so the scheme is only compared when present on both
// sides; the host:port must always match.
func sameDestination(current, expected string) bool {
	curScheme, curHostPort := splitScheme(current)
	expScheme, expHostPort := splitScheme(expected)
	if curHostPort != expHostPort {
		return false
	}
	if curScheme != "" && expScheme != "" {
		return curScheme == expScheme
	}
	return true
}

// splitScheme separates an optional "scheme://" prefix from a destination,
// returning the scheme (without "://") and the remaining "host:port".
func splitScheme(dest string) (scheme, hostPort string) {
	if i := strings.Index(dest, "://"); i >= 0 {
		return dest[:i], dest[i+3:]
	}
	return "", dest
}

// buildDestination constructs the destination URL for a service
func buildDestination(svc *apptypes.ContainerService) string {
	// Use the service protocol directly in the destination URL
	// The protocol flag and destination protocol should match the service configuration
	return fmt.Sprintf("%s://%s:%s", svc.Protocol, svc.IPAddress, svc.TargetPort)
}

// serviceConfigChanged reports whether the advertised endpoint differs from the
// desired container labels. Destination, Tailscale-facing protocol, and PROXY
// protocol version are all compared so enabling, changing, or removing
// --proxy-protocol is picked up on the next reconcile.
func serviceConfigChanged(current ServiceEndpoint, desired *apptypes.ContainerService) bool {
	expectedDest := buildDestination(desired)
	return !sameDestination(current.Destination, expectedDest) ||
		current.Protocol != desired.ServiceProtocol ||
		current.ProxyProtocol != desired.ProxyProtocol
}
