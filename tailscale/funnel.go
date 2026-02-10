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

// FunnelStatus represents the JSON structure from 'tailscale funnel status --json'
type FunnelStatus struct {
	TCP         map[string]map[string]bool `json:"TCP"`
	Web         map[string]FunnelWebConfig `json:"Web"`
	AllowFunnel map[string]bool            `json:"AllowFunnel"`
}

type FunnelWebConfig struct {
	Handlers map[string]FunnelHandler `json:"Handlers"`
}

type FunnelHandler struct {
	Proxy string `json:"Proxy"`
}

// getCurrentFunnels retrieves the current funnel status
// Returns a map where the value is the port (e.g., "443") for cleanup
func (c *Client) getCurrentFunnels(ctx context.Context) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "funnel", "status", "--json")
	output, err := cmd.CombinedOutput()

	// Funnel status command doesn't exist or no funnels configured
	// This is expected when funnel isn't being used
	if err != nil || len(output) == 0 {
		log.Debug().Msg("No funnels configured (this is normal if funnel is not in use)")
		return make(map[string]string), nil
	}

	// Strip warnings from output (like we do for serve status)
	outputStr := stripWarnings(output)

	// Check if output indicates no funnels (before trying to parse JSON)
	if isNotFoundError(outputStr) || len(outputStr) == 0 || outputStr == "\n" {
		log.Debug().Msg("No existing funnels found")
		return make(map[string]string), nil
	}

	// Parse JSON output
	var status FunnelStatus
	if err := json.Unmarshal([]byte(outputStr), &status); err != nil {
		log.Warn().Err(err).Str("output", outputStr).Msg("Failed to parse funnel status JSON, assuming no funnels")
		return make(map[string]string), nil
	}

	// Extract ports from AllowFunnel section
	// Format: "hostname.tailnet.ts.net:443" -> true
	funnels := make(map[string]string)
	for hostPort := range status.AllowFunnel {
		// Extract port from "hostname.tailnet.ts.net:443"
		parts := strings.Split(hostPort, ":")
		if len(parts) == 2 {
			port := parts[1]
			funnels[hostPort] = port
			log.Debug().
				Str("host_port", hostPort).
				Str("port", port).
				Msg("Detected active funnel")
		}
	}

	log.Debug().
		Int("funnel_count", len(funnels)).
		Msg("Retrieved current funnel status")

	return funnels, nil
}

// reconcileFunnels manages funnel configuration for all desired services
// Funnel is INDEPENDENT of serve and can be configured separately
func (c *Client) reconcileFunnels(ctx context.Context, desiredServices []*apptypes.ContainerService) error {
	log.Debug().
		Int("service_count", len(desiredServices)).
		Msg("Reconciling funnel configurations")

	// Get current funnel status
	currentFunnels, err := c.getCurrentFunnels(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get current funnels, will proceed with desired state")
		currentFunnels = make(map[string]string) // service:port -> port
	}

	// Build map of desired funnels and check for duplicate funnel-ports
	// Tailscale limitation: only ONE funnel can be active per funnel-port
	desiredFunnels := make(map[string]*apptypes.ContainerService)
	funnelPortUsage := make(map[string]string) // funnel-port -> container name
	var duplicatePortErrors []string

	for _, svc := range desiredServices {
		if svc.FunnelEnabled {
			key := fmt.Sprintf("svc:%s", svc.ServiceName)
			desiredFunnels[key] = svc

			// Check for duplicate funnel-port usage
			if existingContainer, exists := funnelPortUsage[svc.FunnelFunnelPort]; exists {
				errMsg := fmt.Sprintf(
					"funnel-port %s conflict: containers '%s' and '%s' cannot share the same funnel-port (Tailscale limitation: only ONE funnel per port)",
					svc.FunnelFunnelPort, existingContainer, svc.ContainerName,
				)
				duplicatePortErrors = append(duplicatePortErrors, errMsg)
				log.Error().
					Str("funnel_port", svc.FunnelFunnelPort).
					Str("container1", existingContainer).
					Str("container2", svc.ContainerName).
					Msg("Duplicate funnel-port detected - only one funnel can be active per port")
			} else {
				funnelPortUsage[svc.FunnelFunnelPort] = svc.ContainerName
			}
		}
	}

	// If there are duplicate port errors, log them all and return error
	if len(duplicatePortErrors) > 0 {
		for _, errMsg := range duplicatePortErrors {
			log.Error().Msg(errMsg)
		}
		return fmt.Errorf("funnel configuration error: %d containers have conflicting funnel-ports (only ONE funnel allowed per port)", len(duplicatePortErrors))
	}

	// Find funnels to add
	for serviceName, svc := range desiredFunnels {
		currentPort, exists := currentFunnels[serviceName]

		if !exists || currentPort != svc.FunnelFunnelPort {
			// Funnel doesn't exist or port changed - add/update it
			if exists {
				// Remove old funnel first if port changed
				log.Info().
					Str("container", svc.ContainerName).
					Str("old_public_port", currentPort).
					Str("new_public_port", svc.FunnelFunnelPort).
					Msg("Funnel port changed, updating")
				if err := c.removeFunnel(ctx, svc.ContainerName, currentPort); err != nil {
					log.Error().Err(err).Str("container", svc.ContainerName).Msg("Failed to remove old funnel")
				}
			}

			log.Info().
				Str("container", svc.ContainerName).
				Str("public_port", svc.FunnelFunnelPort).
				Msg("Enabling funnel")

			if err := c.addFunnel(ctx, svc); err != nil {
				log.Error().
					Err(err).
					Str("container", svc.ContainerName).
					Msg("Failed to enable funnel")
				// Continue with other services
			}
		} else {
			log.Debug().
				Str("container", svc.ContainerName).
				Str("public_port", svc.FunnelFunnelPort).
				Msg("Funnel already configured correctly")
		}
	}

	// Find funnels to remove (in current but not in desired)
	// Note: We track by public port (funnel-port) since funnel doesn't use service names
	for _, port := range currentFunnels {
		portInUse := false
		for _, svc := range desiredFunnels {
			if svc.FunnelFunnelPort == port {
				portInUse = true
				break
			}
		}

		if !portInUse {
			log.Info().
				Str("public_port", port).
				Msg("Disabling funnel (no longer desired)")

			if err := c.removeFunnel(ctx, "unknown", port); err != nil {
				log.Error().
					Err(err).
					Str("public_port", port).
					Msg("Failed to disable funnel")
				// Continue with other services
			}
		}
	}

	return nil
}

// addFunnel enables Tailscale Funnel for a service (public internet access)
// Funnel is INDEPENDENT of serve - uses the machine's hostname, not service names
// Exposes at: https://<machine-hostname>.<tailnet>.ts.net:<funnel-port>
func (c *Client) addFunnel(ctx context.Context, svc *apptypes.ContainerService) error {
	if !svc.FunnelEnabled {
		return nil
	}

	// Build destination using funnel's own target port
	funnelDestination := fmt.Sprintf("http://%s:%s", svc.IPAddress, svc.FunnelTargetPort)

	var cmd *exec.Cmd

	// Build funnel command based on protocol
	// Note: Funnel uses machine hostname, NOT service names
	switch svc.FunnelProtocol {
	case "https", "http":
		// HTTPS funnel: tailscale funnel --bg --https=<funnel-port> http://localhost:<host-port>
		portArg := fmt.Sprintf("--https=%s", svc.FunnelFunnelPort)
		cmd = exec.CommandContext(ctx, "tailscale", "funnel", "--bg", portArg, funnelDestination)

	case "tcp":
		// TCP funnel: tailscale funnel --bg --tcp=<funnel-port> tcp://localhost:<host-port>
		portArg := fmt.Sprintf("--tcp=%s", svc.FunnelFunnelPort)
		tcpDest := fmt.Sprintf("tcp://%s:%s", svc.IPAddress, svc.FunnelTargetPort)
		cmd = exec.CommandContext(ctx, "tailscale", "funnel", "--bg", portArg, tcpDest)

	case "tls-terminated-tcp":
		// TLS-terminated TCP funnel
		portArg := fmt.Sprintf("--tls-terminated-tcp=%s", svc.FunnelFunnelPort)
		tcpDest := fmt.Sprintf("tcp://%s:%s", svc.IPAddress, svc.FunnelTargetPort)
		cmd = exec.CommandContext(ctx, "tailscale", "funnel", "--bg", portArg, tcpDest)

	default:
		return fmt.Errorf("unsupported funnel protocol: %s", svc.FunnelProtocol)
	}

	log.Debug().
		Str("command", cmd.String()).
		Str("container", svc.ContainerName).
		Str("funnel_protocol", svc.FunnelProtocol).
		Str("funnel_container_port", svc.FunnelPort).
		Str("funnel_host_port", svc.FunnelTargetPort).
		Str("funnel_public_port", svc.FunnelFunnelPort).
		Str("destination", funnelDestination).
		Msg("Executing tailscale funnel command (uses machine hostname, not service name)")

	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		return fmt.Errorf("failed to enable funnel: %w\nOutput: %s", err, stderr)
	}

	log.Info().
		Str("container", svc.ContainerName).
		Str("public_port", svc.FunnelFunnelPort).
		Str("protocol", svc.FunnelProtocol).
		Msg("Funnel enabled - publicly accessible at https://<machine-hostname>.<tailnet>.ts.net:" + svc.FunnelFunnelPort)

	return nil
}

// removeFunnel disables Tailscale Funnel using reset
// This removes ALL public internet access (funnel is independent of serve and service names)
// Note: tailscale funnel reset removes ALL funnel configs, not just a specific port
func (c *Client) removeFunnel(ctx context.Context, containerName string, port string) error {
	log.Info().
		Str("container", containerName).
		Str("port", port).
		Msg("Disabling funnel - removing public internet access")

	// Command: tailscale funnel reset
	// Note: This resets ALL funnel configuration, not just one port
	cmd := exec.CommandContext(ctx, "tailscale", "funnel", "reset")

	log.Debug().
		Str("command", cmd.String()).
		Str("container", containerName).
		Str("port", port).
		Msg("Executing tailscale funnel reset command")

	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		// Ignore errors if funnel doesn't exist
		if isNotFoundError(stderr) {
			log.Debug().
				Str("container", containerName).
				Str("port", port).
				Msg("Funnel doesn't exist, nothing to remove")
			return nil
		}
		return fmt.Errorf("failed to disable funnel: %w\nOutput: %s", err, stderr)
	}

	log.Info().
		Str("container", containerName).
		Str("port", port).
		Msg("Funnel disabled successfully")

	return nil
}
