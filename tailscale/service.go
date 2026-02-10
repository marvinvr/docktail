package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// GetCurrentServices retrieves the current Tailscale service status using CLI
func (c *Client) GetCurrentServices(ctx context.Context) (map[string]ServiceEndpoint, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "status", "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		// Empty config is not an error
		if isNotFoundError(stderr) {
			log.Debug().Msg("No existing Tailscale services found")
			return make(map[string]ServiceEndpoint), nil
		}
		return nil, fmt.Errorf("failed to get tailscale status: %w (output: %s)", err, stderr)
	}

	// Strip any warning messages from the output
	outputStr := stripWarnings(output)

	// Parse the status JSON
	var status TailscaleStatus
	if err := json.Unmarshal([]byte(outputStr), &status); err != nil {
		// If we can't parse JSON, assume no services
		log.Warn().
			Err(err).
			Str("output", outputStr).
			Msg("Could not parse status JSON, assuming no services")
		return make(map[string]ServiceEndpoint), nil
	}

	log.Debug().
		Int("total_services_in_status", len(status.Services)).
		Msg("Parsed Tailscale status JSON")

	services := make(map[string]ServiceEndpoint)

	// Parse each service
	for serviceName, svcConfig := range status.Services {
		// Only process services we manage (with svc: prefix)
		if !isManagedService(serviceName) {
			continue
		}

		// Parse TCP config to get port and protocol
		for port, tcpConfig := range svcConfig.TCP {
			var protocol string
			if tcpConfig.HTTPS {
				protocol = "https"
			} else if tcpConfig.HTTP {
				protocol = "http"
			} else {
				protocol = "tcp"
			}

			// Get destination from Web config
			var destination string
			for webKey, webConfig := range svcConfig.Web {
				// Find the matching port in the web key
				if strings.Contains(webKey, ":"+port) {
					for _, handler := range webConfig.Handlers {
						if handler.Proxy != "" {
							destination = handler.Proxy
							break
						}
					}
					break
				}
			}

			// Create a unique key for this service+port combination
			key := fmt.Sprintf("%s:%s", serviceName, port)

			services[key] = ServiceEndpoint{
				ServiceName: serviceName,
				Port:        port,
				Protocol:    protocol,
				Destination: destination,
			}

			log.Debug().
				Str("service", serviceName).
				Str("port", port).
				Str("protocol", protocol).
				Str("destination", destination).
				Msg("Parsed existing service")
		}
	}

	log.Info().
		Int("service_count", len(services)).
		Msg("Retrieved current Tailscale services")

	return services, nil
}

// addService adds a single service using Tailscale CLI
// NOTE: This does NOT drain by default - draining only happens when needed
// If adding fails due to config conflict, it clears (with drain) and retries
func (c *Client) addService(ctx context.Context, svc *apptypes.ContainerService) error {
	serviceName := fmt.Sprintf("svc:%s", svc.ServiceName)
	destination := buildDestination(svc)

	// Map service protocol to CLI flag (this is what Tailscale exposes)
	var protocolFlag string
	switch svc.ServiceProtocol {
	case "http":
		protocolFlag = "--http"
	case "https":
		protocolFlag = "--https"
	case "tcp", "tls-terminated-tcp":
		protocolFlag = "--tcp"
	default:
		return fmt.Errorf("unsupported service protocol: %s", svc.ServiceProtocol)
	}

	// Build the command: tailscale serve --service=svc:<name> --<protocol>=<port> <destination>
	portArg := fmt.Sprintf("%s=%s", protocolFlag, svc.Port)
	serviceArg := fmt.Sprintf("--service=%s", serviceName)

	cmd := exec.CommandContext(ctx, "tailscale", "serve", serviceArg, portArg, destination)

	log.Debug().
		Str("command", cmd.String()).
		Str("service", serviceName).
		Str("service_protocol", svc.ServiceProtocol).
		Str("service_port", svc.Port).
		Str("backend_protocol", svc.Protocol).
		Str("destination", destination).
		Msg("Executing tailscale serve command")

	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)

		// Check if error is due to config conflict (e.g., protocol change)
		if isConfigConflictError(stderr) {
			log.Warn().
				Str("service", serviceName).
				Str("error", stderr).
				Msg("Service config conflict detected, clearing old config and retrying")

			// Clear the old service (this will drain connections gracefully)
			if clearErr := c.clearServiceOnly(ctx, serviceName); clearErr != nil {
				return fmt.Errorf("failed to clear conflicting service: %w", clearErr)
			}

			// Retry the add
			log.Info().
				Str("service", serviceName).
				Msg("Retrying add after clearing conflicting config")

			retryCmd := exec.CommandContext(ctx, "tailscale", "serve", serviceArg, portArg, destination)
			retryOutput, retryErr := retryCmd.CombinedOutput()
			if retryErr != nil {
				return fmt.Errorf("failed to add service after clearing: %w\nOutput: %s", retryErr, string(retryOutput))
			}

			log.Info().
				Str("service", serviceName).
				Msg("Service added successfully after resolving conflict")
			return nil
		}

		if isUntaggedNodeError(stderr) {
			return fmt.Errorf("failed to add service: your Tailscale node is not tagged. " +
				"Tailscale Services require the host node to advertise ACL tags.\n" +
				"To fix this:\n" +
				"  1. Tag your Tailscale node:\n" +
				"     - Host install: sudo tailscale up --advertise-tags=tag:server --reset\n" +
				"     - Sidecar container: set TS_EXTRA_ARGS=--advertise-tags=tag:server in your Tailscale container's environment\n" +
				"     - Or tag it in the Tailscale admin console: https://login.tailscale.com/admin/machines → click your node → Edit ACL tags\n" +
				"  2. Add an ACL auto-approver at https://login.tailscale.com/admin/acls:\n" +
				"     \"autoApprovers\": { \"services\": { \"tag:container\": [\"tag:server\"] } }\n" +
				"  3. Approve the service at https://login.tailscale.com/admin/services\n" +
				"Full setup guide: https://github.com/marvinvr/docktail#tailscale-admin-setup")
		}

		return fmt.Errorf("failed to add service: %w\nOutput: %s", err, stderr)
	}

	log.Debug().
		Str("output", string(output)).
		Str("service", serviceName).
		Msg("Service added successfully")

	return nil
}

// clearServiceOnly clears a service configuration without draining
// Used when updating service config (protocol change, etc) where service continues running
func (c *Client) clearServiceOnly(ctx context.Context, serviceName string) error {
	log.Info().
		Str("service", serviceName).
		Msg("Clearing service configuration (no drain - service will be reconfigured)")

	cmd := exec.CommandContext(ctx, "tailscale", "serve", "clear", serviceName)

	log.Debug().
		Str("command", cmd.String()).
		Str("service", serviceName).
		Msg("Executing tailscale serve clear command")

	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		// Ignore errors if service doesn't exist
		if isNotFoundError(stderr) {
			log.Debug().
				Str("service", serviceName).
				Msg("Service doesn't exist, nothing to clear")
			return nil
		}
		return fmt.Errorf("failed to clear service: %w\nOutput: %s", err, stderr)
	}

	log.Info().
		Str("service", serviceName).
		Msg("Service configuration cleared successfully")

	return nil
}

// removeService gracefully removes a service using Tailscale CLI
// It first drains the service (allows existing connections to complete),
// then clears it (removes the configuration)
// SAFETY: Only removes services with "svc:" prefix to avoid touching manually created services
// NOTE: This is used when containers STOP - for config changes, use clearServiceOnly instead
func (c *Client) removeService(ctx context.Context, serviceName string) error {
	// Safety check: only remove services we manage (those with svc: prefix)
	if !isManagedService(serviceName) {
		log.Warn().
			Str("service", serviceName).
			Msg("Refusing to remove service without 'svc:' prefix - not managed by DockTail")
		return fmt.Errorf("refusing to remove service '%s': not managed by DockTail (missing 'svc:' prefix)", serviceName)
	}

	log.Info().
		Str("service", serviceName).
		Msg("Gracefully removing service: draining then clearing")

	// Step 1: Drain the service to gracefully close existing connections
	// This is important for security - prevents stale services from staying accessible
	drainCmd := exec.CommandContext(ctx, "tailscale", "serve", "drain", serviceName)

	log.Debug().
		Str("command", drainCmd.String()).
		Str("service", serviceName).
		Msg("Draining service to close existing connections")

	drainOutput, drainErr := drainCmd.CombinedOutput()
	if drainErr != nil {
		stderr := string(drainOutput)
		// Only warn if drain fails - we'll still try to clear
		if !isNotFoundError(stderr) {
			log.Warn().
				Err(drainErr).
				Str("service", serviceName).
				Str("output", stderr).
				Msg("Failed to drain service, will attempt to clear anyway")
		} else {
			log.Debug().
				Str("service", serviceName).
				Msg("Service doesn't exist for draining, will skip to clear")
		}
	} else {
		log.Info().
			Str("service", serviceName).
			Msg("Service drained successfully")
	}

	// Step 2: Clear the service configuration
	clearCmd := exec.CommandContext(ctx, "tailscale", "serve", "clear", serviceName)

	log.Debug().
		Str("command", clearCmd.String()).
		Str("service", serviceName).
		Msg("Clearing service configuration")

	clearOutput, clearErr := clearCmd.CombinedOutput()
	if clearErr != nil {
		stderr := string(clearOutput)
		// Ignore errors if service doesn't exist
		if isNotFoundError(stderr) {
			log.Debug().
				Str("service", serviceName).
				Msg("Service already removed or doesn't exist")
			return nil
		}
		return fmt.Errorf("failed to clear service: %w\nOutput: %s", clearErr, stderr)
	}

	log.Info().
		Str("service", serviceName).
		Msg("Service removed successfully (drained and cleared)")

	return nil
}

// DrainService gracefully drains a service
func (c *Client) DrainService(ctx context.Context, serviceName string) error {
	fullName := fmt.Sprintf("svc:%s", serviceName)
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "drain", fullName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to drain service %s: %w\nOutput: %s", fullName, err, string(output))
	}
	log.Info().Str("service", fullName).Msg("Drained service")
	return nil
}
