package docker

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
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

// indexedPortRegex matches labels like "docktail.service.1.port", "docktail.service.2.port", etc.
// Each indexed entry defines a separate Tailscale service (requires docktail.service.N.name).
var indexedPortRegex = regexp.MustCompile(`^docktail\.service\.(\d+)\.port$`)

// containerCtx holds shared container context used across multi-port parsing.
type containerCtx struct {
	containerID      string
	containerName    string
	specifiedNetwork string
	inspect          container.InspectResponse
	tags             []string
	destIP           string
	isHostNetwork    bool
	isNoNetwork      bool
	isDirectMode     bool
}

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
		parsed, err := c.parseContainer(ctx, cont.ID, cont.Labels)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container_id", cont.ID[:12]).
				Str("container_name", strings.TrimPrefix(cont.Names[0], "/")).
				Msg("Failed to parse container, skipping")
			continue
		}
		services = append(services, parsed...)
	}

	return services, nil
}

// resolveProtocols applies smart defaults for container protocol, service port, and service protocol.
// Returns (protocol, servicePort, serviceProtocol, error).
func resolveProtocols(containerID, targetPort, servicePort, serviceProtocol, protocol string) (string, string, string, error) {
	// Smart defaults for target/container protocol based on CONTAINER port
	if protocol == "" {
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
		return "", "", "", fmt.Errorf("invalid protocol: %s (must be http, https, https+insecure, tcp, or tls-terminated-tcp)", protocol)
	}

	// Smart defaults based on both fields
	if servicePort == "" && serviceProtocol == "" {
		if protocol == "tcp" || protocol == "tls-terminated-tcp" {
			servicePort = "80"
			serviceProtocol = protocol
			log.Debug().
				Str("container", containerID[:12]).
				Str("backend_protocol", protocol).
				Msg("No port or service protocol specified, defaulting to TCP on port 80 to match backend")
		} else {
			servicePort = "80"
			serviceProtocol = "http"
			log.Debug().
				Str("container", containerID[:12]).
				Msg("No port or protocol specified, defaulting to HTTP on port 80")
		}
	} else if servicePort == "" && serviceProtocol != "" {
		switch serviceProtocol {
		case "https":
			servicePort = "443"
		default:
			servicePort = "80"
		}
		log.Debug().
			Str("container", containerID[:12]).
			Str("service_protocol", serviceProtocol).
			Str("defaulted_service_port", servicePort).
			Msg("Service port not specified, defaulted based on protocol")
	} else if servicePort != "" && serviceProtocol == "" {
		if protocol == "tcp" || protocol == "tls-terminated-tcp" {
			serviceProtocol = protocol
			log.Debug().
				Str("container", containerID[:12]).
				Str("service_port", servicePort).
				Str("backend_protocol", protocol).
				Str("defaulted_service_protocol", serviceProtocol).
				Msg("Service protocol not specified, defaulted to match backend TCP protocol")
		} else {
			switch servicePort {
			case "443":
				serviceProtocol = "https"
			case "80":
				serviceProtocol = "http"
			default:
				serviceProtocol = "http"
			}
			log.Debug().
				Str("container", containerID[:12]).
				Str("service_port", servicePort).
				Str("defaulted_service_protocol", serviceProtocol).
				Msg("Service protocol not specified, defaulted based on port")
		}
	}

	// Validate service protocol
	validServiceProtocols := map[string]bool{
		"http":               true,
		"https":              true,
		"tcp":                true,
		"tls-terminated-tcp": true,
	}
	if !validServiceProtocols[serviceProtocol] {
		return "", "", "", fmt.Errorf("invalid service-protocol: %s (must be http, https, tcp, or tls-terminated-tcp)", serviceProtocol)
	}

	return protocol, servicePort, serviceProtocol, nil
}

// resolveDestPort determines the destination IP and port based on networking mode.
// Returns (destIP, destPort, error).
func (c *Client) resolveDestPort(cctx *containerCtx, targetPort string) (string, string, error) {
	if cctx.isHostNetwork {
		log.Info().
			Str("container", cctx.containerName).
			Str("port", targetPort).
			Msg("Container uses host networking, port is directly accessible on localhost")
		return "localhost", targetPort, nil
	}

	if cctx.isDirectMode {
		if cctx.isNoNetwork {
			return "", "", fmt.Errorf("container '%s' uses network_mode: none, cannot use direct mode", cctx.containerName)
		}

		containerIP, networkName, err := c.getContainerIP(cctx.inspect, cctx.specifiedNetwork, cctx.containerName)
		if err != nil {
			return "", "", err
		}

		if err := c.checkReachability(containerIP, targetPort); err != nil {
			log.Debug().
				Str("container", cctx.containerName).
				Str("container_ip", containerIP).
				Str("port", targetPort).
				Msg("Container not yet reachable (may still be starting)")
		}

		log.Info().
			Str("container", cctx.containerName).
			Str("container_ip", containerIP).
			Str("container_port", targetPort).
			Str("network", networkName).
			Str("will_proxy_to", fmt.Sprintf("%s:%s", containerIP, targetPort)).
			Msg("Proxying directly to container IP (no port publishing required)")

		return containerIP, targetPort, nil
	}

	// Published port mode
	targetPortKey := nat.Port(fmt.Sprintf("%s/tcp", targetPort))
	var hostPort string

	log.Debug().
		Str("container", cctx.containerName).
		Str("looking_for_port", string(targetPortKey)).
		Msg("Direct mode disabled, looking for published port binding")

	if cctx.inspect.HostConfig != nil && cctx.inspect.HostConfig.PortBindings != nil {
		if bindings, ok := cctx.inspect.HostConfig.PortBindings[targetPortKey]; ok && len(bindings) > 0 {
			hostPort = bindings[0].HostPort
			log.Debug().
				Str("container", cctx.containerName).
				Str("target_port", targetPort).
				Str("host_port", hostPort).
				Msg("Detected published port binding")
		}
	}

	if hostPort == "" && cctx.inspect.NetworkSettings != nil && cctx.inspect.NetworkSettings.Ports != nil {
		if bindings, ok := cctx.inspect.NetworkSettings.Ports[targetPortKey]; ok && len(bindings) > 0 {
			hostPort = bindings[0].HostPort
			log.Debug().
				Str("container", cctx.containerName).
				Str("target_port", targetPort).
				Str("host_port", hostPort).
				Msg("Detected published port from NetworkSettings")
		}
	}

	if hostPort == "" {
		var availablePorts []string
		if cctx.inspect.HostConfig != nil && cctx.inspect.HostConfig.PortBindings != nil {
			for port := range cctx.inspect.HostConfig.PortBindings {
				availablePorts = append(availablePorts, string(port))
			}
		}

		log.Warn().
			Str("container", cctx.containerName).
			Str("needed_port", string(targetPortKey)).
			Strs("available_ports", availablePorts).
			Msg("Port not found in bindings (direct mode is disabled)")

		return "", "", fmt.Errorf(
			"container port %s is NOT published to host (direct mode disabled via docktail.service.direct=false). "+
				"Fix: Add 'ports: [\"%s:%s\"]' to container '%s' in docker-compose.yaml, "+
				"or remove 'docktail.service.direct=false' to use container IP directly. "+
				"Available published ports: %v",
			targetPort, targetPort, targetPort, cctx.containerName, availablePorts,
		)
	}

	log.Info().
		Str("container", cctx.containerName).
		Str("container_port", targetPort).
		Str("host_port", hostPort).
		Str("will_proxy_to", fmt.Sprintf("localhost:%s", hostPort)).
		Msg("Direct mode disabled - using published port binding")

	return "localhost", hostPort, nil
}

// parseContainer extracts service configuration from container labels.
// Returns one ContainerService for the primary port plus one for each indexed port.
func (c *Client) parseContainer(ctx context.Context, containerID string, labels map[string]string) ([]*apptypes.ContainerService, error) {
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

	// Resolve protocols for the primary port
	protocol, port, serviceProtocol, err := resolveProtocols(
		containerID, targetPort,
		labels[apptypes.LabelPort],
		labels[apptypes.LabelServiceProtocol],
		labels[apptypes.LabelTargetProtocol],
	)
	if err != nil {
		return nil, err
	}

	// Get container details for port bindings
	inspect, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	containerName := strings.TrimPrefix(inspect.Name, "/")

	cctx := &containerCtx{
		containerID:      containerID,
		containerName:    containerName,
		specifiedNetwork: labels[apptypes.LabelNetwork],
		inspect:          inspect,
		isHostNetwork:    inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "host",
		isNoNetwork:      inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "none",
		isDirectMode:     labels[apptypes.LabelDirect] != "false",
	}

	// Resolve destination for primary port
	destIP, destPort, err := c.resolveDestPort(cctx, targetPort)
	if err != nil {
		return nil, err
	}

	// Parse tags
	var tags []string
	if tagsStr := labels[apptypes.LabelTags]; tagsStr != "" {
		parts := strings.Split(tagsStr, ",")
		for _, part := range parts {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				if !strings.HasPrefix(trimmed, "tag:") {
					log.Warn().
						Str("container", cctx.containerName).
						Str("tag", trimmed).
						Msg("Tag should start with 'tag:' prefix per Tailscale convention")
				}
				tags = append(tags, trimmed)
			}
		}
	} else {
		tags = make([]string, len(c.defaultTags))
		copy(tags, c.defaultTags)
	}
	cctx.tags = tags
	cctx.destIP = destIP

	// Parse funnel configuration (COMPLETELY INDEPENDENT of serve)
	funnelEnabled := labels[apptypes.LabelFunnelEnable] == "true"
	var funnelPort, funnelTargetPort, funnelFunnelPort, funnelProtocol string

	if funnelEnabled {
		funnelPort = labels[apptypes.LabelFunnelPort]
		if funnelPort == "" {
			return nil, fmt.Errorf("funnel enabled but missing required label: %s (container port)", apptypes.LabelFunnelPort)
		}

		funnelProtocol = labels[apptypes.LabelFunnelProtocol]
		if funnelProtocol == "" {
			funnelProtocol = "https"
			log.Debug().
				Str("container", containerID[:12]).
				Msg("Funnel protocol not specified, defaulting to HTTPS")
		}

		funnelFunnelPort = labels[apptypes.LabelFunnelFunnelPort]
		if funnelFunnelPort == "" {
			funnelFunnelPort = "443"
			log.Debug().
				Str("container", containerID[:12]).
				Msg("Funnel public port not specified, defaulting to 443")
		}

		if funnelProtocol == "https" || funnelProtocol == "http" {
			validFunnelPorts := map[string]bool{"443": true, "8443": true, "10000": true}
			if !validFunnelPorts[funnelFunnelPort] {
				return nil, fmt.Errorf("invalid funnel-port: %s for HTTPS/HTTP (must be 443, 8443, or 10000)", funnelFunnelPort)
			}
		}

		validFunnelProtocols := map[string]bool{"https": true, "tcp": true, "tls-terminated-tcp": true}
		if !validFunnelProtocols[funnelProtocol] {
			return nil, fmt.Errorf("invalid funnel protocol: %s (must be https, tcp, or tls-terminated-tcp)", funnelProtocol)
		}

		if cctx.isHostNetwork {
			funnelTargetPort = funnelPort
		} else if cctx.isDirectMode {
			funnelTargetPort = funnelPort
		} else {
			funnelPortKey := nat.Port(fmt.Sprintf("%s/tcp", funnelPort))
			if cctx.inspect.HostConfig != nil && cctx.inspect.HostConfig.PortBindings != nil {
				if bindings, ok := cctx.inspect.HostConfig.PortBindings[funnelPortKey]; ok && len(bindings) > 0 {
					funnelTargetPort = bindings[0].HostPort
				}
			}
			if funnelTargetPort == "" && cctx.inspect.NetworkSettings != nil && cctx.inspect.NetworkSettings.Ports != nil {
				if bindings, ok := cctx.inspect.NetworkSettings.Ports[funnelPortKey]; ok && len(bindings) > 0 {
					funnelTargetPort = bindings[0].HostPort
				}
			}
			if funnelTargetPort == "" {
				return nil, fmt.Errorf("funnel container port %s is NOT published to host (direct mode disabled). Add it to ports in docker-compose, or remove 'docktail.service.direct=false'", funnelPort)
			}
		}

		log.Info().
			Str("container", cctx.containerName).
			Str("funnel_container_port", funnelPort).
			Str("funnel_host_port", funnelTargetPort).
			Str("funnel_public_port", funnelFunnelPort).
			Str("funnel_protocol", funnelProtocol).
			Msg("Funnel enabled for public internet access")
	}

	// Build primary service
	primary := &apptypes.ContainerService{
		ContainerID:      cctx.containerID[:12],
		ContainerName:    cctx.containerName,
		ServiceName:      serviceName,
		Port:             port,
		TargetPort:       destPort,
		ServiceProtocol:  serviceProtocol,
		Protocol:         protocol,
		Tags:             tags,
		IPAddress:        destIP,
		FunnelEnabled:    funnelEnabled,
		FunnelPort:       funnelPort,
		FunnelTargetPort: funnelTargetPort,
		FunnelFunnelPort: funnelFunnelPort,
		FunnelProtocol:   funnelProtocol,
	}

	// Parse indexed services (one container can define multiple separate Tailscale services)
	indexedServices, err := c.parseIndexedPorts(cctx, labels, serviceName, port)
	if err != nil {
		return nil, err
	}

	result := make([]*apptypes.ContainerService, 0, 1+len(indexedServices))
	result = append(result, primary)
	result = append(result, indexedServices...)

	return result, nil
}

// parseIndexedPorts scans labels for indexed service definitions (docktail.service.N.*)
// and returns a ContainerService for each valid index. Each index defines a separate
// Tailscale service and requires its own name (docktail.service.N.name).
func (c *Client) parseIndexedPorts(
	cctx *containerCtx,
	labels map[string]string,
	primaryServiceName string,
	primaryServicePort string,
) ([]*apptypes.ContainerService, error) {
	// Collect all indices from labels
	indices := map[int]bool{}
	for key := range labels {
		if matches := indexedPortRegex.FindStringSubmatch(key); matches != nil {
			idx, err := strconv.Atoi(matches[1])
			if err != nil {
				continue
			}
			indices[idx] = true
		}
	}

	if len(indices) == 0 {
		return nil, nil
	}

	// Sort indices for deterministic processing
	sorted := make([]int, 0, len(indices))
	for idx := range indices {
		sorted = append(sorted, idx)
	}
	sort.Ints(sorted)

	log.Info().
		Str("container", cctx.containerName).
		Int("indexed_services", len(sorted)).
		Msg("Found indexed service definitions")

	// Track service name+port combos to detect duplicates.
	// Scoped by service name so different services can use the same port.
	usedServicePorts := map[string]int{}
	usedServicePorts[primaryServiceName+":"+primaryServicePort] = 0

	var services []*apptypes.ContainerService
	for _, idx := range sorted {
		prefix := fmt.Sprintf("docktail.service.%d.", idx)

		idxServiceName := labels[prefix+"name"]
		if idxServiceName == "" {
			log.Warn().
				Str("container", cctx.containerName).
				Int("index", idx).
				Msg("Missing required name label for indexed service, skipping")
			continue
		}

		targetPort := labels[prefix+"port"]
		if targetPort == "" {
			continue
		}

		idxServicePort := labels[prefix+"service-port"]
		idxServiceProtocol := labels[prefix+"service-protocol"]
		idxProtocol := labels[prefix+"protocol"]

		protocol, servicePort, serviceProtocol, err := resolveProtocols(
			cctx.containerID, targetPort, idxServicePort, idxServiceProtocol, idxProtocol,
		)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Msg("Failed to resolve protocols for indexed service, skipping")
			continue
		}

		// Check for duplicate service name + port combo
		dedupKey := idxServiceName + ":" + servicePort
		if prevIdx, exists := usedServicePorts[dedupKey]; exists {
			log.Warn().
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Int("conflicts_with", prevIdx).
				Str("service_port", servicePort).
				Msg("Duplicate service name and port across indices, skipping")
			continue
		}
		usedServicePorts[dedupKey] = idx

		// Resolve destination port
		idxDestIP, idxDestPort, err := c.resolveDestPort(cctx, targetPort)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Str("target_port", targetPort).
				Msg("Failed to resolve destination for indexed service, skipping")
			continue
		}

		// In direct mode, reuse the primary port's container IP to avoid redundant
		// getContainerIP calls — all ports on the same container share one IP.
		if cctx.isDirectMode && cctx.destIP != "" {
			idxDestIP = cctx.destIP
		}

		svc := &apptypes.ContainerService{
			ContainerID:     cctx.containerID[:12],
			ContainerName:   cctx.containerName,
			ServiceName:     idxServiceName,
			Port:            servicePort,
			TargetPort:      idxDestPort,
			ServiceProtocol: serviceProtocol,
			Protocol:        protocol,
			Tags:            cctx.tags,
			IPAddress:       idxDestIP,
			FunnelEnabled:   false,
		}

		services = append(services, svc)

		log.Info().
			Str("container", cctx.containerName).
			Str("service", idxServiceName).
			Int("index", idx).
			Str("target_port", targetPort).
			Str("service_port", servicePort).
			Str("service_protocol", serviceProtocol).
			Str("protocol", protocol).
			Msg("Parsed indexed service")
	}

	return services, nil
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
