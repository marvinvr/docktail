package reconciler

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/docker"
	"github.com/marvinvr/docktail/tailscale"
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
		event := log.Debug().
			Str("container", container.ContainerName).
			Bool("service_enabled", container.ServiceEnabled).
			Bool("funnel_enabled", container.FunnelEnabled).
			Str("ip", container.IPAddress)

		if container.ServiceEnabled {
			event = event.
				Str("service", container.ServiceName).
				Str("port", container.Port).
				Str("target", container.TargetPort).
				Str("protocol", container.Protocol)
		}

		if container.FunnelEnabled {
			event = event.
				Str("funnel_public_port", container.FunnelFunnelPort).
				Str("funnel_target_port", container.FunnelTargetPort).
				Str("funnel_protocol", container.FunnelProtocol)
		}

		event.Msg("Container configuration")
	}

	// Reconcile services using CLI commands
	// This will compare current state with desired state and make incremental changes
	// When containers stop, their services are gracefully drained (existing connections complete)
	// then cleared (configuration removed) for security
	if err := r.tailscaleClient.ReconcileServices(ctx, containers); err != nil {
		return fmt.Errorf("failed to reconcile services: %w", err)
	}

	log.Info().Msg("Reconciliation completed successfully")
	return nil
}
