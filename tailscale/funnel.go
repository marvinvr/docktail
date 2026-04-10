package tailscale

import (
	"context"
	"encoding/json"
	"errors"
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

type CurrentFunnel struct {
	PublicPort  string
	Protocol    string
	Destination string
}

func extractPort(value string) string {
	idx := strings.LastIndex(value, ":")
	if idx == -1 || idx == len(value)-1 {
		return ""
	}
	return value[idx+1:]
}

func detectFunnelProtocol(config map[string]bool) string {
	for key, enabled := range config {
		if !enabled {
			continue
		}

		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", "-"), " ", "-"))
		switch {
		case strings.Contains(normalized, "tls") && strings.Contains(normalized, "tcp"):
			return "tls-terminated-tcp"
		case strings.Contains(normalized, "https") || strings.Contains(normalized, "http"):
			return "https"
		case strings.Contains(normalized, "tcp"):
			return "tcp"
		}
	}

	return ""
}

func firstProxy(config FunnelWebConfig) string {
	for _, handler := range config.Handlers {
		if handler.Proxy != "" {
			return handler.Proxy
		}
	}
	return ""
}

func normalizeDesiredFunnelProtocol(protocol string) string {
	if protocol == "http" {
		return "https"
	}
	return protocol
}

func desiredFunnelDestination(svc *apptypes.ContainerService) string {
	switch svc.FunnelProtocol {
	case "tcp", "tls-terminated-tcp":
		return fmt.Sprintf("tcp://%s:%s", svc.IPAddress, svc.FunnelTargetPort)
	default:
		return fmt.Sprintf("http://%s:%s", svc.IPAddress, svc.FunnelTargetPort)
	}
}

func currentFunnelMatchesDesired(current CurrentFunnel, svc *apptypes.ContainerService) bool {
	if current.PublicPort == "" {
		return false
	}

	if current.Protocol == "" && current.Destination == "" {
		return false
	}

	if current.Protocol != "" && current.Protocol != normalizeDesiredFunnelProtocol(svc.FunnelProtocol) {
		return false
	}

	if current.Destination != "" && current.Destination != desiredFunnelDestination(svc) {
		return false
	}

	return true
}

func isFunnelACLError(output string) bool {
	return strings.Contains(output, "list of allowed nodes in the tailnet policy file does not include") ||
		strings.Contains(output, "Funnel is enabled, but the list of allowed nodes")
}

func managedFunnelPortSet(ports map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(ports))
	for port := range ports {
		cloned[port] = struct{}{}
	}
	return cloned
}

// getCurrentFunnels retrieves the current funnel status
// Returns a map keyed by public port (for example "443").
func (c *Client) getCurrentFunnels(ctx context.Context) (map[string]CurrentFunnel, error) {
	cmd := c.tailscaleCmd(ctx, "funnel", "status", "--json")
	output, err := cmd.CombinedOutput()

	if err != nil {
		outputStr := string(output)
		if len(outputStr) == 0 || isNotFoundError(outputStr) {
			log.Debug().Msg("No funnels configured (this is normal if funnel is not in use)")
			return make(map[string]CurrentFunnel), nil
		}
		return nil, fmt.Errorf("failed to get funnel status: %w\nOutput: %s", err, outputStr)
	}

	if len(output) == 0 {
		log.Debug().Msg("No funnels configured (this is normal if funnel is not in use)")
		return make(map[string]CurrentFunnel), nil
	}

	// Strip warnings from output (like we do for serve status)
	outputStr := stripWarnings(output)

	// Check if output indicates no funnels (before trying to parse JSON)
	if isNotFoundError(outputStr) || len(outputStr) == 0 || outputStr == "\n" {
		log.Debug().Msg("No existing funnels found")
		return make(map[string]CurrentFunnel), nil
	}

	// Parse JSON output
	var status FunnelStatus
	if err := json.Unmarshal([]byte(outputStr), &status); err != nil {
		log.Warn().Err(err).Str("output", outputStr).Msg("Failed to parse funnel status JSON, assuming no funnels")
		return make(map[string]CurrentFunnel), nil
	}

	funnels := make(map[string]CurrentFunnel)
	for hostPort := range status.AllowFunnel {
		port := extractPort(hostPort)
		if port == "" {
			continue
		}

		current := funnels[port]
		current.PublicPort = port
		current.Protocol = detectFunnelProtocol(status.TCP[port])
		funnels[port] = current

		log.Debug().
			Str("host_port", hostPort).
			Str("port", port).
			Msg("Detected active funnel")
	}

	for port, tcpConfig := range status.TCP {
		current := funnels[port]
		current.PublicPort = port
		if current.Protocol == "" {
			current.Protocol = detectFunnelProtocol(tcpConfig)
		}
		funnels[port] = current
	}

	for webKey, webConfig := range status.Web {
		port := extractPort(webKey)
		if port == "" {
			continue
		}

		current := funnels[port]
		current.PublicPort = port
		if current.Protocol == "" {
			current.Protocol = "https"
		}
		current.Destination = firstProxy(webConfig)
		funnels[port] = current
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
		currentFunnels = make(map[string]CurrentFunnel)
	}

	// Build map of desired funnels and check for duplicate funnel-ports
	// Tailscale limitation: only ONE funnel can be active per funnel-port
	desiredFunnels := make(map[string]*apptypes.ContainerService)
	funnelPortUsage := make(map[string]string) // funnel-port -> container name
	var duplicatePortErrors []string

	for _, svc := range desiredServices {
		if svc.FunnelEnabled {
			key := svc.FunnelFunnelPort
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

	previouslyManaged := managedFunnelPortSet(c.managedFunnels)
	staleManagedFunnels := make([]string, 0)
	unmanagedCurrentFunnels := make([]string, 0)

	for publicPort := range currentFunnels {
		if _, managed := previouslyManaged[publicPort]; managed {
			if _, desired := desiredFunnels[publicPort]; !desired {
				staleManagedFunnels = append(staleManagedFunnels, publicPort)
			}
			continue
		}
		unmanagedCurrentFunnels = append(unmanagedCurrentFunnels, publicPort)
	}

	if len(staleManagedFunnels) > 0 {
		if len(unmanagedCurrentFunnels) > 0 {
			log.Warn().
				Strs("stale_public_ports", staleManagedFunnels).
				Strs("unmanaged_public_ports", unmanagedCurrentFunnels).
				Msg("Skipping stale funnel cleanup because unmanaged funnels exist on this node")
		} else {
			log.Info().
				Strs("public_ports", staleManagedFunnels).
				Msg("Resetting DockTail-managed funnel configuration before applying desired state")

			if err := c.resetFunnels(ctx, "reconcile"); err != nil {
				return err
			}
			currentFunnels = make(map[string]CurrentFunnel)
			staleManagedFunnels = nil
		}
	}

	// Find funnels to add or update.
	var applyErrors []error
	successfulFunnels := make(map[string]struct{}, len(desiredFunnels)+len(staleManagedFunnels))
	for publicPort, svc := range desiredFunnels {
		current, exists := currentFunnels[publicPort]

		if exists && currentFunnelMatchesDesired(current, svc) {
			log.Debug().
				Str("container", svc.ContainerName).
				Str("public_port", svc.FunnelFunnelPort).
				Msg("Funnel already configured correctly")
			successfulFunnels[publicPort] = struct{}{}
			continue
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
			applyErrors = append(applyErrors, fmt.Errorf("%s:%s: %w", svc.ContainerName, publicPort, err))
			continue
		}

		successfulFunnels[publicPort] = struct{}{}
	}

	for _, publicPort := range staleManagedFunnels {
		successfulFunnels[publicPort] = struct{}{}
	}
	c.managedFunnels = successfulFunnels

	if len(applyErrors) > 0 {
		return fmt.Errorf("failed to enable %d funnel(s): %w", len(applyErrors), errors.Join(applyErrors...))
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
	funnelDestination := desiredFunnelDestination(svc)

	var cmd *exec.Cmd

	// Build funnel command based on protocol
	// Note: Funnel uses machine hostname, NOT service names
	switch svc.FunnelProtocol {
	case "https", "http":
		// HTTPS funnel: tailscale funnel --bg --https=<funnel-port> http://localhost:<host-port>
		portArg := fmt.Sprintf("--https=%s", svc.FunnelFunnelPort)
		cmd = c.tailscaleCmd(ctx, "funnel", "--bg", portArg, funnelDestination)

	case "tcp":
		// TCP funnel: tailscale funnel --bg --tcp=<funnel-port> tcp://localhost:<host-port>
		portArg := fmt.Sprintf("--tcp=%s", svc.FunnelFunnelPort)
		tcpDest := fmt.Sprintf("tcp://%s:%s", svc.IPAddress, svc.FunnelTargetPort)
		cmd = c.tailscaleCmd(ctx, "funnel", "--bg", portArg, tcpDest)

	case "tls-terminated-tcp":
		// TLS-terminated TCP funnel
		portArg := fmt.Sprintf("--tls-terminated-tcp=%s", svc.FunnelFunnelPort)
		tcpDest := fmt.Sprintf("tcp://%s:%s", svc.IPAddress, svc.FunnelTargetPort)
		cmd = c.tailscaleCmd(ctx, "funnel", "--bg", portArg, tcpDest)

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
		if isFunnelACLError(stderr) {
			return fmt.Errorf(
				"failed to enable funnel: this node is not allowed by your Tailscale funnel policy.\n"+
					"Add the node to the allowed funnel nodes in your tailnet policy, then retry.\n"+
					"Output: %s",
				stderr,
			)
		}
		return fmt.Errorf("failed to enable funnel: %w\nOutput: %s", err, stderr)
	}

	currentFunnels, verifyErr := c.getCurrentFunnels(ctx)
	if verifyErr != nil {
		return fmt.Errorf("funnel command succeeded but status verification failed: %w", verifyErr)
	}

	current, exists := currentFunnels[svc.FunnelFunnelPort]
	if !exists || !currentFunnelMatchesDesired(current, svc) {
		stderr := string(output)
		if isFunnelACLError(stderr) {
			return fmt.Errorf(
				"tailscale funnel command completed but the funnel was not created because this node is not allowed by your Tailscale funnel policy.\n"+
					"Add the node to the allowed funnel nodes in your tailnet policy, then retry.\n"+
					"Output: %s",
				stderr,
			)
		}
		return fmt.Errorf(
			"tailscale funnel command completed but the requested public port %s is not active.\nOutput: %s",
			svc.FunnelFunnelPort, stderr,
		)
	}

	log.Info().
		Str("container", svc.ContainerName).
		Str("public_port", svc.FunnelFunnelPort).
		Str("protocol", svc.FunnelProtocol).
		Msg("Funnel enabled - publicly accessible at https://<machine-hostname>.<tailnet>.ts.net:" + svc.FunnelFunnelPort)

	return nil
}

// resetFunnels clears all machine-level funnel configuration.
func (c *Client) resetFunnels(ctx context.Context, reason string) error {
	log.Info().
		Str("reason", reason).
		Msg("Resetting funnel configuration")

	cmd := c.tailscaleCmd(ctx, "funnel", "reset")

	log.Debug().
		Str("command", cmd.String()).
		Str("reason", reason).
		Msg("Executing tailscale funnel reset command")

	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		// Ignore errors if funnel doesn't exist
		if isNotFoundError(stderr) {
			log.Debug().
				Str("reason", reason).
				Msg("Funnel doesn't exist, nothing to reset")
			return nil
		}
		return fmt.Errorf("failed to reset funnels: %w\nOutput: %s", err, stderr)
	}

	log.Info().
		Str("reason", reason).
		Msg("Funnel configuration reset successfully")

	return nil
}
