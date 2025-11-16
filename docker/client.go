package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/ts-svc-autopilot/types"
)

// Client wraps the Docker client with our business logic
type Client struct {
	cli *client.Client
}

// NewClient creates a new Docker client
func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &Client{cli: cli}, nil
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

// GetEnabledContainers returns all running containers with ts-svc.enable=true
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
	// Check if autopilot is enabled
	if labels[apptypes.LabelEnable] != "true" {
		return nil, nil
	}

	// Validate required labels
	serviceName := labels[apptypes.LabelService]
	if serviceName == "" {
		return nil, fmt.Errorf("missing required label: %s", apptypes.LabelService)
	}

	port := labels[apptypes.LabelPort]
	if port == "" {
		return nil, fmt.Errorf("missing required label: %s", apptypes.LabelPort)
	}

	targetPort := labels[apptypes.LabelTarget]
	if targetPort == "" {
		return nil, fmt.Errorf("missing required label: %s", apptypes.LabelTarget)
	}

	protocol := labels[apptypes.LabelProtocol]
	if protocol == "" {
		return nil, fmt.Errorf("missing required label: %s", apptypes.LabelProtocol)
	}

	// Validate protocol
	validProtocols := map[string]bool{
		"http":                true,
		"https":               true,
		"tcp":                 true,
		"tls-terminated-tcp":  true,
	}
	if !validProtocols[protocol] {
		return nil, fmt.Errorf("invalid protocol: %s (must be http, https, tcp, or tls-terminated-tcp)", protocol)
	}

	// Get container details for IP address
	inspect, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	// Get IP address from specified network or auto-detect
	networkName := labels[apptypes.LabelNetwork]
	var ipAddress string

	if networkName != "" {
		// Use specified network
		if network, ok := inspect.NetworkSettings.Networks[networkName]; ok {
			ipAddress = network.IPAddress
		}
		if ipAddress == "" {
			return nil, fmt.Errorf("no IP address found on specified network: %s", networkName)
		}
	} else {
		// Auto-detect: try bridge first, then use first available network
		if network, ok := inspect.NetworkSettings.Networks["bridge"]; ok && network.IPAddress != "" {
			ipAddress = network.IPAddress
			networkName = "bridge"
		} else {
			// Use first available network with an IP address
			for name, network := range inspect.NetworkSettings.Networks {
				if network.IPAddress != "" {
					ipAddress = network.IPAddress
					networkName = name
					break
				}
			}
		}

		if ipAddress == "" {
			return nil, fmt.Errorf("no IP address found on any network")
		}
	}

	containerName := strings.TrimPrefix(inspect.Name, "/")

	log.Debug().
		Str("container", containerName).
		Str("network", networkName).
		Str("ip", ipAddress).
		Msg("Detected container network configuration")

	return &apptypes.ContainerService{
		ContainerID:   containerID[:12],
		ContainerName: containerName,
		ServiceName:   serviceName,
		Port:          port,
		TargetPort:    targetPort,
		Protocol:      protocol,
		IPAddress:     ipAddress,
		Network:       networkName,
	}, nil
}
