package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/ts-svc-autopilot/types"
)

// Client handles Tailscale CLI interactions
type Client struct {
	socketPath string
}

// NewClient creates a new Tailscale client
func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
	}
}

// BuildConfig creates a Tailscale service configuration from container services
func (c *Client) BuildConfig(services []*apptypes.ContainerService) *apptypes.TailscaleServiceConfig {
	config := &apptypes.TailscaleServiceConfig{
		Version:  "0.0.1",
		Services: make(map[string]apptypes.ServiceDefinition),
	}

	for _, svc := range services {
		serviceName := fmt.Sprintf("svc:%s", svc.ServiceName)

		// Build endpoint key (e.g., "tcp:443")
		endpointKey := fmt.Sprintf("tcp:%s", svc.Port)

		// Build endpoint value based on protocol
		var endpointValue string
		switch svc.Protocol {
		case "http", "https":
			endpointValue = fmt.Sprintf("%s://%s:%s", svc.Protocol, svc.IPAddress, svc.TargetPort)
		case "tcp", "tls-terminated-tcp":
			endpointValue = fmt.Sprintf("%s://%s:%s", svc.Protocol, svc.IPAddress, svc.TargetPort)
		}

		// Add or merge with existing service
		if existing, ok := config.Services[serviceName]; ok {
			existing.Endpoints[endpointKey] = endpointValue
			config.Services[serviceName] = existing
		} else {
			config.Services[serviceName] = apptypes.ServiceDefinition{
				Endpoints: map[string]string{
					endpointKey: endpointValue,
				},
			}
		}

		log.Debug().
			Str("service", serviceName).
			Str("endpoint", endpointKey).
			Str("target", endpointValue).
			Msg("Added service endpoint")
	}

	return config
}

// GetCurrentConfig retrieves the current Tailscale service configuration
func (c *Client) GetCurrentConfig(ctx context.Context) (*apptypes.TailscaleServiceConfig, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "get-config", "--all", "--json")

	output, err := cmd.CombinedOutput()
	if err != nil {
		stderr := string(output)
		// Empty config is not an error
		if strings.Contains(stderr, "no config") ||
		   strings.Contains(stderr, "not found") ||
		   strings.Contains(stderr, "nothing to show") ||
		   strings.Contains(stderr, "no serve config") {
			log.Debug().Msg("No existing Tailscale serve config found, starting fresh")
			return &apptypes.TailscaleServiceConfig{
				Version:  "0.0.1",
				Services: make(map[string]apptypes.ServiceDefinition),
			}, nil
		}
		return nil, fmt.Errorf("failed to get tailscale config: %w (output: %s)", err, stderr)
	}

	var config apptypes.TailscaleServiceConfig
	if err := json.Unmarshal(output, &config); err != nil {
		return nil, fmt.Errorf("failed to parse tailscale config: %w", err)
	}

	return &config, nil
}

// ApplyConfig applies a Tailscale service configuration
func (c *Client) ApplyConfig(ctx context.Context, config *apptypes.TailscaleServiceConfig) error {
	// Write config to temp file
	tmpFile, err := os.CreateTemp("", "ts-svc-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	encoder := json.NewEncoder(tmpFile)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	tmpFile.Close()

	// Apply config
	cmd := exec.CommandContext(ctx, "tailscale", "serve", "set-config", "--all", tmpFile.Name())
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set tailscale config: %w\nOutput: %s", err, string(output))
	}

	log.Info().Msg("Applied Tailscale service configuration")
	return nil
}

// AdvertiseServices advertises all services in the configuration
func (c *Client) AdvertiseServices(ctx context.Context, config *apptypes.TailscaleServiceConfig) error {
	for serviceName := range config.Services {
		cmd := exec.CommandContext(ctx, "tailscale", "serve", "advertise", serviceName)
		if output, err := cmd.CombinedOutput(); err != nil {
			log.Warn().
				Err(err).
				Str("service", serviceName).
				Str("output", string(output)).
				Msg("Failed to advertise service")
			// Continue with other services
			continue
		}
		log.Info().
			Str("service", serviceName).
			Msg("Advertised service")
	}
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
