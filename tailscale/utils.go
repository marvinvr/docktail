package tailscale

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

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

// buildDestination constructs the destination URL for a service
func buildDestination(svc *apptypes.ContainerService) string {
	// Use the service protocol directly in the destination URL
	// The protocol flag and destination protocol should match the service configuration
	return fmt.Sprintf("%s://%s:%s", svc.Protocol, svc.IPAddress, svc.TargetPort)
}
