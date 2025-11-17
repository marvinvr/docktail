package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/docker"
	"github.com/marvinvr/docktail/tailscale"
	apptypes "github.com/marvinvr/docktail/types"
)

// Reconciler manages the reconciliation loop
type Reconciler struct {
	dockerClient    *docker.Client
	tailscaleClient *tailscale.Client
	interval        time.Duration
}

// NewReconciler creates a new reconciler
func NewReconciler(dockerClient *docker.Client, tailscaleClient *tailscale.Client, interval time.Duration) *Reconciler {
	return &Reconciler{
		dockerClient:    dockerClient,
		tailscaleClient: tailscaleClient,
		interval:        interval,
	}
}

// Run starts the reconciliation loop
func (r *Reconciler) Run(ctx context.Context) error {
	// Initial reconciliation
	if err := r.Reconcile(ctx); err != nil {
		log.Error().Err(err).Msg("Initial reconciliation failed")
	}

	// Start event watcher
	eventsChan, errChan := r.dockerClient.WatchEvents(ctx)

	// Start periodic reconciliation ticker
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-errChan:
			if err != nil {
				log.Error().Err(err).Msg("Docker event stream error")
				// Try to reconnect by continuing
				time.Sleep(5 * time.Second)
				eventsChan, errChan = r.dockerClient.WatchEvents(ctx)
			}

		case event := <-eventsChan:
			log.Debug().
				Str("action", string(event.Action)).
				Str("container", event.Actor.ID[:12]).
				Msg("Docker event received")

			// Trigger reconciliation on relevant events
			if err := r.Reconcile(ctx); err != nil {
				log.Error().Err(err).Msg("Event-triggered reconciliation failed")
			}

		case <-ticker.C:
			log.Debug().Msg("Running periodic reconciliation")
			if err := r.Reconcile(ctx); err != nil {
				log.Error().Err(err).Msg("Periodic reconciliation failed")
			}
		}
	}
}

// Reconcile performs a single reconciliation cycle
func (r *Reconciler) Reconcile(ctx context.Context) error {
	log.Info().Msg("Starting reconciliation")

	// Get all enabled containers from Docker
	containers, err := r.dockerClient.GetEnabledContainers(ctx)
	if err != nil {
		return fmt.Errorf("failed to get enabled containers: %w", err)
	}

	log.Info().
		Int("count", len(containers)).
		Msg("Found enabled containers")

	for _, container := range containers {
		log.Debug().
			Str("container", container.ContainerName).
			Str("service", container.ServiceName).
			Str("ip", container.IPAddress).
			Str("port", container.Port).
			Str("target", container.TargetPort).
			Str("protocol", container.Protocol).
			Msg("Container configuration")
	}

	// Build desired Tailscale configuration from ALL enabled containers
	desiredConfig := r.tailscaleClient.BuildConfig(containers)

	// Log the desired services
	var desiredServices []string
	for svc := range desiredConfig.Services {
		desiredServices = append(desiredServices, svc)
	}
	log.Info().
		Strs("desired_services", desiredServices).
		Int("service_count", len(desiredConfig.Services)).
		Msg("Built desired configuration from containers")

	// Get current Tailscale configuration
	currentConfig, err := r.tailscaleClient.GetCurrentConfig(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get current config, will apply full config")
		currentConfig = &apptypes.TailscaleServiceConfig{
			Version:  "0.0.1",
			Services: make(map[string]apptypes.ServiceDefinition),
		}
	}

	// Log current services
	var currentServices []string
	for svc := range currentConfig.Services {
		currentServices = append(currentServices, svc)
	}
	log.Info().
		Strs("current_services", currentServices).
		Int("service_count", len(currentConfig.Services)).
		Msg("Retrieved current configuration")

	// Check if configuration needs to be updated
	if configsEqual(currentConfig, desiredConfig) {
		log.Info().
			Strs("services", desiredServices).
			Msg("Configuration is up to date, no changes needed")
		return nil
	}

	log.Info().
		Strs("desired_services", desiredServices).
		Strs("current_services", currentServices).
		Msg("Configuration changed, applying full configuration for all services")

	// Apply the complete desired configuration using --all flag
	// This replaces the entire Tailscale serve configuration
	if err := r.tailscaleClient.ApplyConfig(ctx, desiredConfig); err != nil {
		return fmt.Errorf("failed to apply config: %w", err)
	}

	// Advertise all services in the configuration
	if err := r.tailscaleClient.AdvertiseServices(ctx, desiredConfig); err != nil {
		return fmt.Errorf("failed to advertise services: %w", err)
	}

	log.Info().Msg("Reconciliation completed successfully")
	return nil
}

// configsEqual compares two Tailscale configurations
func configsEqual(a, b *apptypes.TailscaleServiceConfig) bool {
	if len(a.Services) != len(b.Services) {
		return false
	}

	for serviceName, aService := range a.Services {
		bService, ok := b.Services[serviceName]
		if !ok {
			return false
		}

		if len(aService.Endpoints) != len(bService.Endpoints) {
			return false
		}

		for endpoint, aTarget := range aService.Endpoints {
			bTarget, ok := bService.Endpoints[endpoint]
			if !ok || aTarget != bTarget {
				return false
			}
		}
	}

	return true
}
