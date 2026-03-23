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
	if c.serverVersion != "" {
		cmd.Env = append(os.Environ(), "TS_DEBUG_FAKE_IPC_VERSION="+c.serverVersion)
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
		if c.serverVersion != "" {
			log.Info().
				Str("previous_server_version", c.serverVersion).
				Msg("Tailscale CLI/daemon versions now aligned; disabling TS_DEBUG_FAKE_IPC_VERSION override")
			c.serverVersion = ""
		}
		return
	}

	matches := versionMismatchRe.FindStringSubmatch(outStr)
	if len(matches) < 2 {
		c.serverVersion = ""
		log.Warn().
			Str("output", outStr).
			Msg("Detected tailscale version mismatch but could not parse server version")
		return
	}

	c.serverVersion = matches[1]
	log.Info().
		Str("server_version", c.serverVersion).
		Msg("Tailscale CLI/daemon version mismatch detected; will use TS_DEBUG_FAKE_IPC_VERSION for CLI calls")
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

// buildDestination constructs the destination URL for a service
func buildDestination(svc *apptypes.ContainerService) string {
	// Use the service protocol directly in the destination URL
	// The protocol flag and destination protocol should match the service configuration
	return fmt.Sprintf("%s://%s:%s", svc.Protocol, svc.IPAddress, svc.TargetPort)
}
