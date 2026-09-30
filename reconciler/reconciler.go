package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/docker"
	"github.com/marvinvr/docktail/tailscale"
	apptypes "github.com/marvinvr/docktail/types"
)

// ErrListContainers marks a reconcile that failed before it could read the
// containers from Docker — Docker is unreachable, as opposed to a failure while
// applying one service.
var ErrListContainers = errors.New("failed to get enabled containers")

// Observer receives reconciler outputs for an optional side-channel consumer —
// the cloud module (see ../cloud). The reconciler calls these inline, so an
// implementation must return quickly (hand off to its own goroutines/queues).
// The reconciler itself never imports the cloud package; this keeps the AGPL
// agent free of any dependency on the cloud's wiring.
type Observer interface {
	// OnReconcile is called after each successful reconcile with the freshly
	// computed services (the same value the reconciler just applied).
	OnReconcile(ctx context.Context, services []*apptypes.ContainerService)
	// OnEvent is called for every docker container event the reconciler observes
	// (the widened set: start/stop/die/restart/oom/health_status).
	OnEvent(ctx context.Context, event events.Message)
}

// Reconciler manages the reconciliation loop
type Reconciler struct {
	dockerClient    *docker.Client
	tailscaleClient *tailscale.Client
	interval        time.Duration
	observer        Observer        // optional; nil unless the cloud module is enabled
	onResult        func(err error) // optional; told the outcome of every reconcile
}

// SetResultHook installs a function told the outcome of every reconcile cycle
// (nil on success) — the local health status. Safe to call once before Run.
func (r *Reconciler) SetResultHook(fn func(err error)) {
	r.onResult = fn
}

// SetObserver attaches an optional observer (the cloud collector). Pass nil to
// detach. Safe to call once before Run.
func (r *Reconciler) SetObserver(o Observer) {
	r.observer = o
}

// triggersReconcile reports whether a docker event action should drive a
// tailscale reconcile. The cloud module observes the wider event set, but only
// these actions change desired service state — health_status/oom must not cause
// reconcile storms.
func triggersReconcile(a events.Action) bool {
	switch a {
	case events.ActionStart, events.ActionStop, events.ActionDie, events.ActionRestart:
		return true
	default:
		return false
	}
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

			// Forward every observed event to the cloud module (if attached).
			if r.observer != nil {
				r.observer.OnEvent(ctx, event)
			}

			// Trigger reconciliation only on events that change desired state.
			if triggersReconcile(event.Action) {
				if err := r.Reconcile(ctx); err != nil {
					log.Error().Err(err).Msg("Event-triggered reconciliation failed")
				}
			}

		case <-ticker.C:
			log.Debug().Msg("Running periodic reconciliation")
			if err := r.Reconcile(ctx); err != nil {
				log.Error().Err(err).Msg("Periodic reconciliation failed")
			}
		}
	}
}

// Reconcile performs a single reconciliation cycle and reports its outcome to
// the result hook, if one is set.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	err := r.reconcile(ctx)
	if r.onResult != nil {
		r.onResult(err)
	}
	return err
}

func (r *Reconciler) reconcile(ctx context.Context) error {
	log.Info().Msg("Starting reconciliation")

	// Get all enabled containers from Docker
	containers, err := r.dockerClient.GetEnabledContainers(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrListContainers, err)
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

	// Hand the freshly computed services to the cloud module (if attached). The
	// cloud also runs its own periodic Docker discovery so stopped containers can
	// stay visible without becoming Tailscale serve targets.
	if r.observer != nil {
		r.observer.OnReconcile(ctx, containers)
	}

	log.Info().Msg("Reconciliation completed successfully")
	return nil
}
