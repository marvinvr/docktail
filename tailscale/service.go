package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// GetCurrentServices retrieves the current Tailscale service status using CLI
func (c *Client) GetCurrentServices(ctx context.Context) (map[string]ServiceEndpoint, error) {
	cmd := c.tailscaleCmd(ctx, "serve", "status", "--json")
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

	services := parseManagedServices(status)

	log.Info().
		Int("service_count", len(services)).
		Msg("Retrieved current Tailscale services")

	return services, nil
}

// parseManagedServices extracts the DockTail-managed service endpoints from a
// parsed Tailscale serve status. It is kept separate from the CLI invocation so
// the parsing logic (in particular how destinations are resolved per protocol)
// can be unit tested directly.
func parseManagedServices(status TailscaleStatus) map[string]ServiceEndpoint {
	services := make(map[string]ServiceEndpoint)

	for serviceName, svcConfig := range status.Services {
		// Only process services we manage (with svc: prefix)
		if !isManagedService(serviceName) {
			continue
		}

		// Parse TCP config to get port, protocol and destination
		for port, tcpConfig := range svcConfig.TCP {
			protocol := serviceProtocol(tcpConfig)
			path, destination := serviceHandler(svcConfig, port, tcpConfig)

			// Create a unique key for this service+port combination
			key := fmt.Sprintf("%s:%s", serviceName, port)

			services[key] = ServiceEndpoint{
				ServiceName:   serviceName,
				Port:          port,
				Protocol:      protocol,
				Path:          path,
				Destination:   destination,
				ProxyProtocol: tcpConfig.ProxyProtocol,
			}

			log.Debug().
				Str("service", serviceName).
				Str("port", port).
				Str("protocol", protocol).
				Str("path", path).
				Str("destination", destination).
				Msg("Parsed existing service")
		}
	}

	return services
}

// serviceProtocol maps a TCP handler's flags to the DockTail protocol name.
func serviceProtocol(tcpConfig TailscaleTCPConfig) string {
	switch {
	case tcpConfig.TerminateTLS != "":
		return "tls-terminated-tcp"
	case tcpConfig.HTTPS:
		return "https"
	case tcpConfig.HTTP:
		return "http"
	default:
		return "tcp"
	}
}

// serviceDestination resolves the backend destination for a service endpoint.
// HTTP/HTTPS endpoints proxy through the Web handler config, whereas plain-TCP
// endpoints forward directly via the TCP handler's TCPForward field. Previously
// the destination was only read from the Web section, which is empty for plain
// TCP services; that left their destination empty and made reconciliation treat
// every TCP service as changed, re-adding it on each cycle (issue #56).
func serviceHandler(svcConfig TailscaleService, port string, tcpConfig TailscaleTCPConfig) (string, string) {
	if !tcpConfig.HTTP && !tcpConfig.HTTPS {
		return "", tcpConfig.TCPForward
	}

	for webKey, webConfig := range svcConfig.Web {
		// Find the matching port in the web key
		if strings.Contains(webKey, ":"+port) {
			for path, handler := range webConfig.Handlers {
				if handler.Proxy != "" {
					return path, handler.Proxy
				}
			}
			break
		}
	}

	return "", ""
}

func serviceDestination(svcConfig TailscaleService, port string, tcpConfig TailscaleTCPConfig) string {
	_, destination := serviceHandler(svcConfig, port, tcpConfig)
	return destination
}

// serveAddArgs builds `tailscale serve` arguments for advertising one service.
// --proxy-protocol is included only when the label requested a version; Tailscale
// rejects that flag on HTTP/HTTPS, so callers must already have validated it.
func serveAddArgs(svc *apptypes.ContainerService) ([]string, error) {
	serviceName := fmt.Sprintf("svc:%s", svc.ServiceName)
	destination := buildDestination(svc)

	var protocolFlag string
	switch svc.ServiceProtocol {
	case "http":
		protocolFlag = "--http"
	case "https":
		protocolFlag = "--https"
	case "tcp":
		protocolFlag = "--tcp"
	case "tls-terminated-tcp":
		protocolFlag = "--tls-terminated-tcp"
	default:
		return nil, fmt.Errorf("unsupported service protocol: %s", svc.ServiceProtocol)
	}

	args := []string{
		"serve",
		fmt.Sprintf("--service=%s", serviceName),
		fmt.Sprintf("%s=%s", protocolFlag, svc.Port),
	}
	if svc.ProxyProtocol != 0 {
		args = append(args, fmt.Sprintf("--proxy-protocol=%d", svc.ProxyProtocol))
	}
	if svc.ServiceProtocol == "http" || svc.ServiceProtocol == "https" {
		args = append(args, fmt.Sprintf("--set-path=%s", normalizeServicePath(svc.ServicePath)))
	}
	args = append(args, destination)
	return args, nil
}

// addService adds a single service using Tailscale CLI
// NOTE: This does NOT drain by default - draining only happens when needed
// If adding fails due to config conflict, it clears (with drain) and retries
func (c *Client) addService(ctx context.Context, svc *apptypes.ContainerService) error {
	args, err := serveAddArgs(svc)
	if err != nil {
		return err
	}
	serviceName := fmt.Sprintf("svc:%s", svc.ServiceName)
	destination := buildDestination(svc)

	cmd := c.tailscaleCmd(ctx, args...)

	log.Debug().
		Str("command", cmd.String()).
		Str("service", serviceName).
		Str("service_protocol", svc.ServiceProtocol).
		Str("service_port", svc.Port).
		Str("backend_protocol", svc.Protocol).
		Int("proxy_protocol", svc.ProxyProtocol).
		Str("service_path", svc.ServicePath).
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

			retryCmd := c.tailscaleCmd(ctx, args...)
			retryOutput, retryErr := retryCmd.CombinedOutput()
			if retryErr != nil {
				if isServeConfigDeniedError(string(retryOutput)) {
					return errors.New(serveConfigDeniedMessage("add service after clearing"))
				}
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
				"  2. Define tags and add an ACL auto-approver at https://login.tailscale.com/admin/acls:\n" +
				"     \"tagOwners\": { \"tag:server\": [\"autogroup:admin\"], \"tag:container\": [\"tag:server\"] }\n" +
				"     \"autoApprovers\": { \"services\": { \"tag:container\": [\"tag:server\"] } }\n" +
				"  3. Approve the service at https://login.tailscale.com/admin/services\n" +
				"Full setup guide: https://github.com/marvinvr/docktail#tailscale-admin-setup")
		}

		if isServeConfigDeniedError(stderr) {
			return errors.New(serveConfigDeniedMessage("add service"))
		}

		return fmt.Errorf("failed to add service: %w\nOutput: %s", err, stderr)
	}

	log.Debug().
		Str("output", string(output)).
		Str("service", serviceName).
		Msg("Service added successfully")

	return nil
}

// removeServicePath removes one HTTP(S) handler before a path change. A serve
// invocation adds or updates a mount point but does not remove the previous
// one, so the original flags must be replayed with "off".
func (c *Client) removeServicePath(ctx context.Context, svc ServiceEndpoint) error {
	args, err := serveRemovePathArgs(svc)
	if err != nil {
		return err
	}
	cmd := c.tailscaleCmd(ctx, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to remove old service path: %w\nOutput: %s", err, string(output))
	}
	return nil
}

func serveRemovePathArgs(svc ServiceEndpoint) ([]string, error) {
	if svc.Protocol != "http" && svc.Protocol != "https" {
		return nil, fmt.Errorf("service path is unsupported for protocol %s", svc.Protocol)
	}
	return []string{
		"serve",
		fmt.Sprintf("--service=%s", svc.ServiceName),
		fmt.Sprintf("--%s=%s", svc.Protocol, svc.Port),
		fmt.Sprintf("--set-path=%s", normalizeServicePath(svc.Path)),
		"off",
	}, nil
}

// clearServiceOnly clears a service configuration without draining
// Used when updating service config (protocol change, etc) where service continues running
func (c *Client) clearServiceOnly(ctx context.Context, serviceName string) error {
	log.Info().
		Str("service", serviceName).
		Msg("Clearing service configuration (no drain - service will be reconfigured)")

	cmd := c.tailscaleCmd(ctx, "serve", "clear", serviceName)

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

	if c.shouldIgnoreService(serviceName) {
		log.Info().
			Str("service", serviceName).
			Msg("Refusing to remove ignored service")
		return nil
	}

	log.Info().
		Str("service", serviceName).
		Msg("Gracefully removing service: draining then clearing")

	// Step 1: Drain the service to gracefully close existing connections
	// This is important for security - prevents stale services from staying accessible
	drainCmd := c.tailscaleCmd(ctx, "serve", "drain", serviceName)

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
	clearCmd := c.tailscaleCmd(ctx, "serve", "clear", serviceName)

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
	cmd := c.tailscaleCmd(ctx, "serve", "drain", fullName)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to drain service %s: %w\nOutput: %s", fullName, err, string(output))
	}
	log.Info().Str("service", fullName).Msg("Drained service")
	return nil
}
