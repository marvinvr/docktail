package docker

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// Client wraps the Docker client with our business logic
type Client struct {
	cli         *client.Client
	defaultTags []string
}

// NewClient creates a new Docker client
func NewClient(defaultTags []string) (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &Client{cli: cli, defaultTags: defaultTags}, nil
}

// Close closes the Docker client
func (c *Client) Close() error {
	return c.cli.Close()
}

// WatchEvents streams Docker container events
func (c *Client) WatchEvents(ctx context.Context) (<-chan events.Message, <-chan error) {
	eventsChan, errChan := c.cli.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "container"),
			filters.Arg("event", "start"),
			filters.Arg("event", "stop"),
			filters.Arg("event", "die"),
			filters.Arg("event", "restart"),
		),
	})

	return eventsChan, errChan
}

// GetEnabledContainers returns all running containers with docktail.service.enable=true
func (c *Client) GetEnabledContainers(ctx context.Context) ([]*apptypes.ContainerService, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", apptypes.LabelEnable+"=true"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var services []*apptypes.ContainerService
	for _, cont := range containers {
		service, err := c.parseContainer(ctx, cont.ID, cont.Labels)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container_id", cont.ID[:12]).
				Str("container_name", strings.TrimPrefix(cont.Names[0], "/")).
				Msg("Failed to parse container, skipping")
			continue
		}
		if service != nil {
			services = append(services, service)
		}
	}

	return services, nil
}

// parseContainer extracts service configuration from container labels
func (c *Client) parseContainer(ctx context.Context, containerID string, labels map[string]string) (*apptypes.ContainerService, error) {
	// Check if docktail is enabled
	if labels[apptypes.LabelEnable] != "true" {
		return nil, nil
	}

	// Validate required labels
	serviceName := labels[apptypes.LabelService]
	if serviceName == "" {
		return nil, fmt.Errorf("missing required label: %s", apptypes.LabelService)
	}

	targetPort := labels[apptypes.LabelTarget]
	if targetPort == "" {
		return nil, fmt.Errorf("missing required label: %s", apptypes.LabelTarget)
	}

	// Optional labels with smart defaults - these work in both directions:
	// - If service-port=443 and service-protocol unset → defaults to HTTPS
	// - If service-protocol=https and service-port unset → defaults to 443
	port := labels[apptypes.LabelPort]
	serviceProtocol := labels[apptypes.LabelServiceProtocol]

	// Smart defaults for target/container protocol based on CONTAINER port
	// This needs to be parsed FIRST since it affects service protocol defaults
	protocol := labels[apptypes.LabelTargetProtocol]
	if protocol == "" {
		// Default based on container port
		switch targetPort {
		case "443":
			protocol = "https"
		default:
			protocol = "http"
		}
		log.Debug().
			Str("container", containerID[:12]).
			Str("container_port", targetPort).
			Str("defaulted_protocol", protocol).
			Msg("Container protocol not specified, defaulted based on container port")
	}

	// Validate target protocol
	validProtocols := map[string]bool{
		"http":               true,
		"https":              true,
		"https+insecure":     true,
		"tcp":                true,
		"tls-terminated-tcp": true,
	}
	if !validProtocols[protocol] {
		return nil, fmt.Errorf("invalid protocol: %s (must be http, https, https+insecure, tcp, or tls-terminated-tcp)", protocol)
	}

	// Smart defaults based on both fields
	// IMPORTANT: When backend protocol is TCP, service protocol should also default to TCP
	if port == "" && serviceProtocol == "" {
		// Both unset: default based on backend protocol
		if protocol == "tcp" || protocol == "tls-terminated-tcp" {
			port = "80"
			serviceProtocol = protocol // Use same protocol as backend for TCP
			log.Debug().
				Str("container", containerID[:12]).
				Str("backend_protocol", protocol).
				Msg("No port or service protocol specified, defaulting to TCP on port 80 to match backend")
		} else {
			port = "80"
			serviceProtocol = "http"
			log.Debug().
				Str("container", containerID[:12]).
				Msg("No port or protocol specified, defaulting to HTTP on port 80")
		}
	} else if port == "" && serviceProtocol != "" {
		// Protocol set, port unset: default port based on protocol
		switch serviceProtocol {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			port = "80"
		}
		log.Debug().
			Str("container", containerID[:12]).
			Str("service_protocol", serviceProtocol).
			Str("defaulted_service_port", port).
			Msg("Service port not specified, defaulted based on protocol")
	} else if port != "" && serviceProtocol == "" {
		// Port set, protocol unset: default protocol based on backend protocol first, then port
		if protocol == "tcp" || protocol == "tls-terminated-tcp" {
			serviceProtocol = protocol // Use same protocol as backend for TCP
			log.Debug().
				Str("container", containerID[:12]).
				Str("service_port", port).
				Str("backend_protocol", protocol).
				Str("defaulted_service_protocol", serviceProtocol).
				Msg("Service protocol not specified, defaulted to match backend TCP protocol")
		} else {
			// For HTTP/HTTPS backends, default based on port
			switch port {
			case "443":
				serviceProtocol = "https"
			case "80":
				serviceProtocol = "http"
			default:
				serviceProtocol = "http"
			}
			log.Debug().
				Str("container", containerID[:12]).
				Str("service_port", port).
				Str("defaulted_service_protocol", serviceProtocol).
				Msg("Service protocol not specified, defaulted based on port")
		}
	}
	// else: both are set, use as-is

	// Validate service protocol (Tailscale-facing protocol)
	validServiceProtocols := map[string]bool{
		"http":               true,
		"https":              true,
		"tcp":                true,
		"tls-terminated-tcp": true,
	}
	if !validServiceProtocols[serviceProtocol] {
		return nil, fmt.Errorf("invalid service-protocol: %s (must be http, https, tcp, or tls-terminated-tcp)", serviceProtocol)
	}

	// Get container details for port bindings
	inspect, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	containerName := strings.TrimPrefix(inspect.Name, "/")

	// Check if container uses host networking
	isHostNetwork := inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "host"
	// Check if container uses no networking
	isNoNetwork := inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "none"

	// Direct container IP proxying is enabled by default
	// Set docktail.service.direct=false to use published port bindings instead
	isDirectMode := labels[apptypes.LabelDirect] != "false"
	specifiedNetwork := labels[apptypes.LabelNetwork]

	// Variables for destination configuration
	var destIP string
	var destPort string

	if isHostNetwork {
		// For host networking, the container port IS the host port on localhost
		destIP = "localhost"
		destPort = targetPort
		log.Info().
			Str("container", containerName).
			Str("port", targetPort).
			Msg("Container uses host networking, port is directly accessible on localhost")
	} else if isDirectMode {
		// Direct mode: proxy to container IP instead of published host port
		if isNoNetwork {
			return nil, fmt.Errorf("container '%s' uses network_mode: none, cannot use direct mode", containerName)
		}

		// Get container IP from network settings
		containerIP, networkName, err := c.getContainerIP(inspect, specifiedNetwork, containerName)
		if err != nil {
			return nil, err
		}

		destIP = containerIP
		destPort = targetPort // Use container port directly

		// Optional reachability check - just for debugging, doesn't block configuration
		if err := c.checkReachability(containerIP, targetPort); err != nil {
			log.Debug().
				Str("container", containerName).
				Str("container_ip", containerIP).
				Str("port", targetPort).
				Msg("Container not yet reachable (may still be starting)")
		}

		log.Info().
			Str("container", containerName).
			Str("container_ip", containerIP).
			Str("container_port", targetPort).
			Str("network", networkName).
			Str("will_proxy_to", fmt.Sprintf("%s:%s", containerIP, targetPort)).
			Msg("Proxying directly to container IP (no port publishing required)")
	} else {
		// Direct mode disabled (docktail.service.direct=false) - need published port bindings
		targetPortKey := nat.Port(fmt.Sprintf("%s/tcp", targetPort))
		var hostPort string

		log.Debug().
			Str("container", containerName).
			Str("looking_for_port", string(targetPortKey)).
			Msg("Direct mode disabled, looking for published port binding")

		if inspect.HostConfig != nil && inspect.HostConfig.PortBindings != nil {
			if bindings, ok := inspect.HostConfig.PortBindings[targetPortKey]; ok && len(bindings) > 0 {
				// Use the first host port binding
				hostPort = bindings[0].HostPort
				log.Debug().
					Str("container", containerName).
					Str("target_port", targetPort).
					Str("host_port", hostPort).
					Msg("Detected published port binding")
			}
		}

		// If no port binding found, check NetworkSettings.Ports as fallback
		if hostPort == "" && inspect.NetworkSettings != nil && inspect.NetworkSettings.Ports != nil {
			if bindings, ok := inspect.NetworkSettings.Ports[targetPortKey]; ok && len(bindings) > 0 {
				hostPort = bindings[0].HostPort
				log.Debug().
					Str("container", containerName).
					Str("target_port", targetPort).
					Str("host_port", hostPort).
					Msg("Detected published port from NetworkSettings")
			}
		}

		if hostPort == "" {
			// Debug: Show what ports ARE available
			var availablePorts []string
			if inspect.HostConfig != nil && inspect.HostConfig.PortBindings != nil {
				for port := range inspect.HostConfig.PortBindings {
					availablePorts = append(availablePorts, string(port))
				}
			}

			log.Warn().
				Str("container", containerName).
				Str("needed_port", string(targetPortKey)).
				Strs("available_ports", availablePorts).
				Msg("Port not found in bindings (direct mode is disabled)")

			return nil, fmt.Errorf(
				"container port %s is NOT published to host (direct mode disabled via docktail.service.direct=false). "+
					"Fix: Add 'ports: [\"%s:%s\"]' to container '%s' in docker-compose.yaml, "+
					"or remove 'docktail.service.direct=false' to use container IP directly. "+
					"Available published ports: %v",
				targetPort, targetPort, targetPort, containerName, availablePorts,
			)
		}

		destIP = "localhost"
		destPort = hostPort

		log.Info().
			Str("container", containerName).
			Str("container_port", targetPort).
			Str("host_port", hostPort).
			Str("will_proxy_to", fmt.Sprintf("localhost:%s", hostPort)).
			Msg("Direct mode disabled - using published port binding")
	}

	// Parse tags
	var tags []string
	if tagsStr := labels[apptypes.LabelTags]; tagsStr != "" {
		// Split by comma and trim spaces
		parts := strings.Split(tagsStr, ",")
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				// Warn if tag doesn't follow Tailscale convention
				if !strings.HasPrefix(trimmed, "tag:") {
					log.Warn().
						Str("container", containerName).
						Str("tag", trimmed).
						Msg("Tag should start with 'tag:' prefix per Tailscale convention")
				}
				tags = append(tags, trimmed)
			}
		}
	} else {
		// Use default tags if no override provided
		tags = make([]string, len(c.defaultTags))
		copy(tags, c.defaultTags)
	}

	// Parse funnel configuration (COMPLETELY INDEPENDENT of serve)
	funnelEnabled := labels[apptypes.LabelFunnelEnable] == "true"
	var funnelPort, funnelTargetPort, funnelFunnelPort, funnelProtocol string

	if funnelEnabled {
		// Get funnel-specific container port (like service.port but for funnel)
		funnelPort = labels[apptypes.LabelFunnelPort]
		if funnelPort == "" {
			return nil, fmt.Errorf("funnel enabled but missing required label: %s (container port)", apptypes.LabelFunnelPort)
		}

		// Get funnel protocol
		funnelProtocol = labels[apptypes.LabelFunnelProtocol]
		if funnelProtocol == "" {
			funnelProtocol = "https" // Default to HTTPS
			log.Debug().
				Str("container", containerID[:12]).
				Msg("Funnel protocol not specified, defaulting to HTTPS")
		}

		// Get public-facing funnel port (funnel-port)
		funnelFunnelPort = labels[apptypes.LabelFunnelFunnelPort]
		if funnelFunnelPort == "" {
			funnelFunnelPort = "443" // Default to 443
			log.Debug().
				Str("container", containerID[:12]).
				Msg("Funnel public port not specified, defaulting to 443")
		}

		// Validate funnel-port for HTTPS (must be 443, 8443, or 10000)
		if funnelProtocol == "https" || funnelProtocol == "http" {
			validFunnelPorts := map[string]bool{
				"443":   true,
				"8443":  true,
				"10000": true,
			}
			if !validFunnelPorts[funnelFunnelPort] {
				return nil, fmt.Errorf("invalid funnel-port: %s for HTTPS/HTTP (must be 443, 8443, or 10000)", funnelFunnelPort)
			}
		}

		// Validate funnel protocol
		validFunnelProtocols := map[string]bool{
			"https":              true,
			"tcp":                true,
			"tls-terminated-tcp": true,
		}
		if !validFunnelProtocols[funnelProtocol] {
			return nil, fmt.Errorf("invalid funnel protocol: %s (must be https, tcp, or tls-terminated-tcp)", funnelProtocol)
		}

		// Find the published host port for the funnel container port
		if isHostNetwork {
			// For host networking, the container port IS the host port
			funnelTargetPort = funnelPort
		} else if isDirectMode {
			// Direct mode: use container port directly (funnel will use same destIP as service)
			funnelTargetPort = funnelPort
		} else {
			funnelPortKey := nat.Port(fmt.Sprintf("%s/tcp", funnelPort))
			if inspect.HostConfig != nil && inspect.HostConfig.PortBindings != nil {
				if bindings, ok := inspect.HostConfig.PortBindings[funnelPortKey]; ok && len(bindings) > 0 {
					funnelTargetPort = bindings[0].HostPort
				}
			}
			if funnelTargetPort == "" && inspect.NetworkSettings != nil && inspect.NetworkSettings.Ports != nil {
				if bindings, ok := inspect.NetworkSettings.Ports[funnelPortKey]; ok && len(bindings) > 0 {
					funnelTargetPort = bindings[0].HostPort
				}
			}

			if funnelTargetPort == "" {
				return nil, fmt.Errorf("funnel container port %s is NOT published to host (direct mode disabled). Add it to ports in docker-compose, or remove 'docktail.service.direct=false'", funnelPort)
			}
		}

		log.Info().
			Str("container", containerName).
			Str("funnel_container_port", funnelPort).
			Str("funnel_host_port", funnelTargetPort).
			Str("funnel_public_port", funnelFunnelPort).
			Str("funnel_protocol", funnelProtocol).
			Msg("Funnel enabled for public internet access")
	}

	return &apptypes.ContainerService{
		ContainerID:      containerID[:12],
		ContainerName:    containerName,
		ServiceName:      serviceName,
		Port:             port,
		TargetPort:       destPort,
		ServiceProtocol:  serviceProtocol,
		Protocol:         protocol,
		Tags:             tags,
		IPAddress:        destIP,
		FunnelEnabled:    funnelEnabled,
		FunnelPort:       funnelPort,       // Container port for funnel
		FunnelTargetPort: funnelTargetPort, // Host port for funnel (or container port in direct mode)
		FunnelFunnelPort: funnelFunnelPort, // Public port for funnel
		FunnelProtocol:   funnelProtocol,
	}, nil
}

// getContainerIP extracts the container's IP address from the specified or default network
func (c *Client) getContainerIP(inspect container.InspectResponse, specifiedNetwork string, containerName string) (string, string, error) {
	if inspect.NetworkSettings == nil || inspect.NetworkSettings.Networks == nil {
		return "", "", fmt.Errorf("container '%s' has no network settings", containerName)
	}

	networks := inspect.NetworkSettings.Networks

	// If a specific network is specified, use it
	if specifiedNetwork != "" {
		// Try exact match first
		if network, ok := networks[specifiedNetwork]; ok {
			if network.IPAddress == "" {
				return "", "", fmt.Errorf("container '%s' has no IP address on network '%s'", containerName, specifiedNetwork)
			}
			return network.IPAddress, specifiedNetwork, nil
		}

		// Try suffix match (handles docker-compose project prefixes like "projectname_backend")
		for networkName, network := range networks {
			if strings.HasSuffix(networkName, "_"+specifiedNetwork) {
				if network.IPAddress == "" {
					return "", "", fmt.Errorf("container '%s' has no IP address on network '%s'", containerName, networkName)
				}
				log.Debug().
					Str("container", containerName).
					Str("requested", specifiedNetwork).
					Str("matched", networkName).
					Msg("Matched network by suffix (docker-compose prefix detected)")
				return network.IPAddress, networkName, nil
			}
		}

		return "", "", fmt.Errorf("container '%s' is not connected to network '%s' (available: %v)", containerName, specifiedNetwork, getNetworkNames(networks))
	}

	// No network specified - try common defaults then fall back to first available
	// Priority: bridge > first available
	if network, ok := networks["bridge"]; ok && network.IPAddress != "" {
		return network.IPAddress, "bridge", nil
	}

	// Fall back to first available network with an IP
	for networkName, network := range networks {
		if network.IPAddress != "" {
			log.Debug().
				Str("container", containerName).
				Str("network", networkName).
				Str("ip", network.IPAddress).
				Msg("Using first available network for direct mode")
			return network.IPAddress, networkName, nil
		}
	}

	return "", "", fmt.Errorf("container '%s' has no IP address on any network", containerName)
}

// getNetworkNames returns a list of network names from the networks map
func getNetworkNames[V any](networks map[string]V) []string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	return names
}

// checkReachability performs a quick TCP connection test (best-effort, non-blocking)
func (c *Client) checkReachability(ip string, port string) error {
	address := net.JoinHostPort(ip, port)
	conn, err := net.DialTimeout("tcp", address, 1*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
