package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"strings"

	"github.com/marvinvr/docktail/docker"
	"github.com/marvinvr/docktail/reconciler"
	"github.com/marvinvr/docktail/tailscale"
)

func main() {
	// Setup logging
	setupLogging()

	log.Info().Msg("Starting DockTail")

	// Get configuration from environment
	reconcileInterval := getEnvDuration("RECONCILE_INTERVAL", 60*time.Second)
	tailscaleSocket := getEnv("TAILSCALE_SOCKET", "/var/run/tailscale/tailscaled.sock")

	// Control Plane Configuration
	tailscaleAPIKey := getEnv("TAILSCALE_API_KEY", "")
	tailscaleOAuthClientID := getEnv("TAILSCALE_OAUTH_CLIENT_ID", "")
	tailscaleOAuthClientSecret := getEnv("TAILSCALE_OAUTH_CLIENT_SECRET", "")
	tailscaleTailnet := getEnv("TAILSCALE_TAILNET", "-")
	defaultTagsStr := getEnv("DEFAULT_SERVICE_TAGS", "tag:container")

	// Parse default tags
	var defaultTags []string
	for _, tag := range strings.Split(defaultTagsStr, ",") {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			defaultTags = append(defaultTags, trimmed)
		}
	}

	// Determine API sync method for logging
	apiSyncMethod := "disabled"
	if tailscaleOAuthClientID != "" && tailscaleOAuthClientSecret != "" {
		apiSyncMethod = "oauth"
	} else if tailscaleAPIKey != "" {
		apiSyncMethod = "api_key"
	}

	log.Info().
		Dur("reconcile_interval", reconcileInterval).
		Str("tailscale_socket", tailscaleSocket).
		Str("api_sync_method", apiSyncMethod).
		Str("tailnet", tailscaleTailnet).
		Strs("default_tags", defaultTags).
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
		SocketPath:        tailscaleSocket,
		Tailnet:           tailscaleTailnet,
		APIKey:            tailscaleAPIKey,
		OAuthClientID:     tailscaleOAuthClientID,
		OAuthClientSecret: tailscaleOAuthClientSecret,
	})

	log.Info().Msg("Tailscale client initialized")

	// Create reconciler
	rec := reconciler.NewReconciler(dockerClient, tailscaleClient, reconcileInterval)

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	// Graceful shutdown: clean up all Tailscale services
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
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
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
