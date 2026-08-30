package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/cloud"
	"github.com/marvinvr/docktail/diag"
	"github.com/marvinvr/docktail/docker"
	"github.com/marvinvr/docktail/reconciler"
	"github.com/marvinvr/docktail/tailscale"
)

// agentVersion is set at build time (see the Dockerfile ldflags) and recorded in
// diagnostics output so a captured log can be tied to a specific build.
var agentVersion = "dev"

func main() {
	// Setup logging
	setupLogging()

	log.Info().Msg("Starting DockTail")

	// Get configuration from environment
	reconcileInterval := getEnvDuration("RECONCILE_INTERVAL", 60*time.Second)
	tailscaleSocket := getEnv("TAILSCALE_SOCKET", "/var/run/tailscale/tailscaled.sock")

	// Control Plane Configuration
	tailscaleAPIKey := getSecretEnv("TAILSCALE_API_KEY", "")
	tailscaleOAuthClientID := getSecretEnv("TAILSCALE_OAUTH_CLIENT_ID", "")
	tailscaleOAuthClientSecret := getSecretEnv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	tailscaleTailnet := getEnv("TAILSCALE_TAILNET", "-")
	defaultTagsStr := getEnv("DEFAULT_SERVICE_TAGS", "tag:container")
	ignoreServiceNamesStr := getEnv("IGNORE_SERVICE_NAMES", "")
	deleteUnusedServices := getEnvBool("DELETE_UNUSED_SERVICES", false)
	skipShutdownCleanup := getEnvBool("SKIP_SHUTDOWN_CLEANUP", false)
	exitOnSocketLoss := getEnvBool("EXIT_ON_SOCKET_LOSS", true)
	socketLossGracePeriod := getEnvDuration("SOCKET_LOSS_GRACE_PERIOD", 90*time.Second)

	// Parse default tags, dropping duplicates: the Control Plane stores tags
	// as a set, so a duplicated default would register as permanent drift and
	// trigger a re-PUT on every reconcile cycle.
	var defaultTags []string
	seenDefaultTags := make(map[string]struct{})
	for _, tag := range strings.Split(defaultTagsStr, ",") {
		trimmed := strings.TrimSpace(tag)
		if trimmed == "" {
			continue
		}
		if _, dup := seenDefaultTags[trimmed]; dup {
			continue
		}
		seenDefaultTags[trimmed] = struct{}{}
		defaultTags = append(defaultTags, trimmed)
	}

	// Parse ignored service names
	var ignoreServiceNames []string
	for _, name := range strings.Split(ignoreServiceNamesStr, ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			ignoreServiceNames = append(ignoreServiceNames, trimmed)
		}
	}

	// Determine API sync method for logging
	apiSyncMethod := "disabled"
	if tailscaleOAuthClientID != "" && tailscaleOAuthClientSecret != "" {
		apiSyncMethod = "oauth"
	} else if tailscaleAPIKey != "" {
		apiSyncMethod = "api_key"
	}

	logCredentialWarnings(tailscaleAPIKey, tailscaleOAuthClientID, tailscaleOAuthClientSecret)

	// The unused-service cleanup relies on the Control Plane API to inspect and
	// delete service definitions. Without credentials it can never run, so warn
	// loudly rather than silently ignoring the opt-in.
	if deleteUnusedServices && apiSyncMethod == "disabled" {
		log.Warn().
			Msg("DELETE_UNUSED_SERVICES is enabled but no Tailscale API credentials are configured; unused-service cleanup will be skipped")
	}

	log.Info().
		Dur("reconcile_interval", reconcileInterval).
		Str("tailscale_socket", tailscaleSocket).
		Str("api_sync_method", apiSyncMethod).
		Str("tailnet", tailscaleTailnet).
		Strs("default_tags", defaultTags).
		Strs("ignore_service_names", ignoreServiceNames).
		Bool("delete_unused_services", deleteUnusedServices).
		Bool("skip_shutdown_cleanup", skipShutdownCleanup).
		Bool("exit_on_socket_loss", exitOnSocketLoss).
		Dur("socket_loss_grace_period", socketLossGracePeriod).
		Msg("Configuration loaded")

	// Create Docker client
	dockerClient, err := docker.NewClient(defaultTags)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to create Docker client")
	}
	defer func() { _ = dockerClient.Close() }()

	log.Info().Msg("Docker client initialized")

	// Create Tailscale client
	tailscaleClient := tailscale.NewClient(tailscale.ClientConfig{
		SocketPath:           tailscaleSocket,
		Tailnet:              tailscaleTailnet,
		APIKey:               tailscaleAPIKey,
		OAuthClientID:        tailscaleOAuthClientID,
		OAuthClientSecret:    tailscaleOAuthClientSecret,
		IgnoreServiceNames:   ignoreServiceNames,
		DeleteUnusedServices: deleteUnusedServices,
	})

	// Detect CLI/daemon version mismatch (common with host-mode Tailscale)
	tailscaleClient.DetectVersionMismatch(context.Background())
	tailscaleClient.WarnIfSocketMissing()

	log.Info().Msg("Tailscale client initialized")

	// Create reconciler
	rec := reconciler.NewReconciler(dockerClient, tailscaleClient, reconcileInterval)

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Optional: DockTail Cloud reporting module. Completely inert unless
	// DOCKTAIL_CLOUD_KEY is set — DockTail runs exactly as before without it.
	if cloud.Enabled() {
		cloudCfg := cloud.LoadConfig()
		collector, cerr := cloud.NewCollector(ctx, cloudCfg, dockerClient, tailscaleClient, log.Logger)
		if cerr != nil {
			log.Error().Err(cerr).Msg("DockTail Cloud enabled but failed to initialize; continuing without it")
		} else {
			rec.SetObserver(collector)
			go collector.Run(ctx)
			log.Info().
				Str("endpoint", cloudCfg.URL).
				Str("fingerprint", collector.Fingerprint()).
				Msg("DockTail Cloud reporting enabled")
		}
	}

	// Optional: diagnostics recorder. Inert unless DIAGNOSTICS=true. It records
	// the half of a service's hosting state that the reconciler never reads
	// (prefs.AdvertiseServices), which is what makes a service offline to the
	// tailnet while DockTail's own logs look clean.
	if diag.Enabled() {
		recorder := diag.New(diag.LoadConfig(), tailscaleClient, agentVersion)
		go recorder.Run(ctx)
	}

	// Watch for a tailscaled socket that stops being reachable and never comes
	// back. This is not a transient error to retry: when the host's tailscaled
	// restarts, systemd replaces /run/tailscale with a new directory, and a
	// container that bind-mounts it stays pinned to the old, deleted one. The
	// socket can then never reappear in this mount namespace, so DockTail would
	// otherwise sit there logging errors forever while every service it manages
	// goes stale (issue #72). Only a container restart re-resolves the mount.
	watchdog := tailscaleClient.NewSocketWatchdog(
		tailscale.SocketWatchdogConfig{
			Enabled:  exitOnSocketLoss,
			Grace:    socketLossGracePeriod,
			Interval: 5 * time.Second,
		},
		func(err error, downFor time.Duration) {
			// Exit without the usual shutdown cleanup: it would only issue more
			// CLI calls over the socket that just went away.
			log.Fatal().Err(err).
				Str("socket", tailscaleSocket).
				Dur("unreachable_for", downFor).
				Msg("Tailscale socket has been unreachable for longer than the grace period, exiting so the container restart policy can re-resolve the mount; if this repeats, ensure DockTail is started with a restart policy and see https://docktail.org/docs/#tailscale-socket-loss")
		},
	)
	go watchdog.Run(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Info().Str("signal", sig.String()).Msg("Received shutdown signal, initiating graceful shutdown")
		cancel()
	}()

	// Run reconciler
	log.Info().Msg("Starting reconciliation loop")
	if err := rec.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal().Err(err).Msg("Reconciler failed")
	}

	// Graceful shutdown: optionally clean up all Tailscale services and funnels.
	// When SKIP_SHUTDOWN_CLEANUP is enabled we leave everything advertised so the
	// services stay reachable while DockTail is down. The serve/funnel config lives
	// in tailscaled, not in DockTail, so skipping cleanup keeps it in place.
	if skipShutdownCleanup {
		log.Info().Msg("SKIP_SHUTDOWN_CLEANUP is enabled, leaving Tailscale services and funnels advertised")
		log.Info().Msg("DockTail stopped gracefully")
		return
	}

	log.Info().Msg("Reconciler stopped, cleaning up Tailscale services")

	// Use a new context with timeout for cleanup (don't use cancelled context)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()

	if err := tailscaleClient.CleanupAllServices(cleanupCtx); err != nil {
		log.Error().Err(err).Msg("Failed to clean up all services during shutdown")
	} else {
		log.Info().Msg("Successfully cleaned up all services")
	}

	log.Info().Msg("DockTail stopped gracefully")
}

func setupLogging() {
	// Configure zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	out := os.Stdout
	_, noColor := os.LookupEnv("NO_COLOR") // adheres no-color.org
	isTTY := term.IsTerminal(int(out.Fd()))
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        out,
		TimeFormat: time.RFC3339,
		NoColor:    noColor || !isTTY || os.Getenv("TERM") == "dumb",
	})

	// Set log level from environment
	logLevel := getEnv("LOG_LEVEL", "info")
	switch logLevel {
	case "debug":
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	case "info":
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	case "warn":
		zerolog.SetGlobalLevel(zerolog.WarnLevel)
	case "error":
		zerolog.SetGlobalLevel(zerolog.ErrorLevel)
	default:
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	log.Debug().Str("level", logLevel).Msg("Log level set")
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getSecretEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	for _, fileKey := range []string{"FILE__" + key, key + "_FILE"} {
		value, ok := getEnvFileValue(fileKey)
		if ok {
			return value
		}
	}

	return defaultValue
}

func getEnvFileValue(fileKey string) (string, bool) {
	path := strings.TrimSpace(os.Getenv(fileKey))
	if path == "" {
		return "", false
	}

	content, err := os.ReadFile(path)
	if err != nil {
		log.Fatal().
			Err(err).
			Str("file_env", fileKey).
			Str("path", path).
			Msg("Failed to read environment variable file")
		return "", true
	}

	return strings.TrimRight(string(content), "\r\n"), true
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
		log.Warn().
			Str("key", key).
			Str("value", value).
			Dur("default", defaultValue).
			Msg("Failed to parse duration, using default")
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseBool(value); err == nil {
			return parsed
		}
		log.Warn().
			Str("key", key).
			Str("value", value).
			Bool("default", defaultValue).
			Msg("Failed to parse bool, using default")
	}
	return defaultValue
}

func logCredentialWarnings(tailscaleAPIKey, tailscaleOAuthClientID, tailscaleOAuthClientSecret string) {
	if tailscaleOAuthClientID == "" && tailscaleOAuthClientSecret == "" {
		if tailscaleAPIKey == "" {
			log.Warn().
				Strs("missing", []string{"TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"}).
				Msg("Tailscale OAuth credentials are not configured; DockTail will still run, but auto-service creation is disabled")
		}
		return
	}

	if tailscaleOAuthClientID == "" || tailscaleOAuthClientSecret == "" {
		missing := make([]string, 0, 2)
		if tailscaleOAuthClientID == "" {
			missing = append(missing, "TAILSCALE_OAUTH_CLIENT_ID")
		}
		if tailscaleOAuthClientSecret == "" {
			missing = append(missing, "TAILSCALE_OAUTH_CLIENT_SECRET")
		}

		event := log.Warn().
			Strs("missing", missing)
		if tailscaleAPIKey != "" {
			event = event.Str("fallback", "api_key")
		} else {
			event = event.Str("fallback", "manual_mode")
		}

		event.Msg("Incomplete Tailscale OAuth configuration; both OAuth environment variables must be set to enable OAuth-based auto-service creation")
	}
}
