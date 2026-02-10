package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2/clientcredentials"

	apptypes "github.com/marvinvr/docktail/types"
)

// Client handles Tailscale CLI interactions and API calls
type Client struct {
	socketPath     string
	tailnet        string
	baseURL        string
	httpClient     *http.Client
	apiSyncEnabled bool
}

// ClientConfig holds configuration for creating a Tailscale client
type ClientConfig struct {
	SocketPath        string
	Tailnet           string
	APIKey            string
	OAuthClientID     string
	OAuthClientSecret string
}

// NewClient creates a new Tailscale client
// Prefers OAuth credentials over API key if both are provided
func NewClient(cfg ClientConfig) *Client {
	client := &Client{
		socketPath: cfg.SocketPath,
		tailnet:    cfg.Tailnet,
		baseURL:    "https://api.tailscale.com",
	}

	// Prefer OAuth over API key
	if cfg.OAuthClientID != "" && cfg.OAuthClientSecret != "" {
		oauthConfig := &clientcredentials.Config{
			ClientID:     cfg.OAuthClientID,
			ClientSecret: cfg.OAuthClientSecret,
			TokenURL:     "https://api.tailscale.com/api/v2/oauth/token",
		}
		// The oauth2 client handles token refresh automatically
		client.httpClient = oauthConfig.Client(context.Background())
		client.httpClient.Timeout = 10 * time.Second
		client.apiSyncEnabled = true
		log.Info().Msg("Tailscale API: using OAuth client credentials")
	} else if cfg.APIKey != "" {
		// Fall back to API key with custom transport
		client.httpClient = &http.Client{
			Timeout:   10 * time.Second,
			Transport: &apiKeyTransport{apiKey: cfg.APIKey},
		}
		client.apiSyncEnabled = true
		log.Info().Msg("Tailscale API: using API key")
	} else {
		// No API credentials - API sync disabled
		client.httpClient = &http.Client{
			Timeout: 10 * time.Second,
		}
		client.apiSyncEnabled = false
		log.Info().Msg("Tailscale API: no credentials configured, control plane sync disabled")
	}

	return client
}

// apiKeyTransport adds the API key as a Bearer token to requests
type apiKeyTransport struct {
	apiKey string
}

func (t *apiKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Bearer "+t.apiKey)
	return http.DefaultTransport.RoundTrip(req)
}

// ServiceEndpoint represents a single endpoint for comparison
type ServiceEndpoint struct {
	ServiceName string // e.g., "svc:web"
	Port        string // e.g., "443"
	Protocol    string // e.g., "http", "https", "tcp"
	Destination string // e.g., "http://localhost:9080"
}

// TailscaleStatus represents the structure of 'tailscale serve status --json'
type TailscaleStatus struct {
	Services map[string]TailscaleService `json:"Services"`
}

type TailscaleService struct {
	TCP map[string]TailscaleTCPConfig `json:"TCP"`
	Web map[string]TailscaleWebConfig `json:"Web"`
}

type TailscaleTCPConfig struct {
	HTTP  bool `json:"HTTP"`
	HTTPS bool `json:"HTTPS"`
}

type TailscaleWebConfig struct {
	Handlers map[string]TailscaleHandler `json:"Handlers"`
}

type TailscaleHandler struct {
	Proxy string `json:"Proxy"`
}

// ReconcileServices compares desired services with current services and makes necessary changes
func (c *Client) ReconcileServices(ctx context.Context, desiredServices []*apptypes.ContainerService) error {
	log.Info().
		Int("desired_count", len(desiredServices)).
		Msg("Starting service reconciliation using CLI commands")

	// Build map of desired services for easy lookup
	desiredMap := make(map[string]*apptypes.ContainerService)
	for _, svc := range desiredServices {
		key := fmt.Sprintf("svc:%s:%s", svc.ServiceName, svc.Port)
		desiredMap[key] = svc
	}

	// Get current services
	currentServices, err := c.GetCurrentServices(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get current services, will apply all desired services")
		currentServices = make(map[string]ServiceEndpoint)
	}

	log.Info().
		Int("current_service_count", len(currentServices)).
		Msg("Retrieved current service state from Tailscale")

	// Track what we need to add and remove
	toAdd := make(map[string]*apptypes.ContainerService)
	toRemove := make(map[string]ServiceEndpoint)

	// Find services to add (in desired but not in current, or changed)
	for key, desired := range desiredMap {
		if current, exists := currentServices[key]; !exists {
			// Service doesn't exist - add it
			toAdd[key] = desired
			log.Debug().
				Str("key", key).
				Str("service", desired.ServiceName).
				Msg("Service not found in current state, will add")
		} else {
			// Service exists - check if configuration changed
			expectedDest := buildDestination(desired)
			if current.Destination != expectedDest || current.Protocol != desired.ServiceProtocol {
				toAdd[key] = desired
				log.Info().
					Str("key", key).
					Str("service", desired.ServiceName).
					Str("current_dest", current.Destination).
					Str("expected_dest", expectedDest).
					Str("current_protocol", current.Protocol).
					Str("expected_protocol", desired.ServiceProtocol).
					Msg("Service configuration changed, will update")
			} else {
				// Service exists and matches - no action needed
				log.Debug().
					Str("key", key).
					Str("service", desired.ServiceName).
					Str("protocol", current.Protocol).
					Str("destination", current.Destination).
					Msg("Service already exists with correct configuration, skipping")
			}
		}
	}

	// Find services to remove (in current but not in desired)
	for key, current := range currentServices {
		if _, exists := desiredMap[key]; !exists {
			toRemove[key] = current
		}
	}

	log.Info().
		Int("to_add", len(toAdd)).
		Int("to_remove", len(toRemove)).
		Msg("Calculated reconciliation actions")

	// Remove old services first
	for key, svc := range toRemove {
		log.Info().
			Str("service", svc.ServiceName).
			Str("port", svc.Port).
			Msg("Removing service")

		if err := c.removeService(ctx, svc.ServiceName); err != nil {
			log.Error().
				Err(err).
				Str("service", svc.ServiceName).
				Msg("Failed to remove service")
			// Continue with other services
		} else {
			log.Info().
				Str("key", key).
				Str("service", svc.ServiceName).
				Msg("Successfully removed service")
		}
	}

	// Add new services
	successCount := 0
	failCount := 0

	for key, svc := range toAdd {
		log.Info().
			Str("container", svc.ContainerName).
			Str("service", svc.ServiceName).
			Str("service_port", svc.Port).
			Str("service_protocol", svc.ServiceProtocol).
			Str("backend_protocol", svc.Protocol).
			Str("backend_port", svc.TargetPort).
			Msg("Adding service")

		if err := c.addService(ctx, svc); err != nil {
			failCount++
			log.Error().
				Err(err).
				Str("service", svc.ServiceName).
				Str("container", svc.ContainerName).
				Msg("Failed to add service")
			// Continue with other services
		} else {
			successCount++
			log.Info().
				Str("key", key).
				Str("service", svc.ServiceName).
				Str("container", svc.ContainerName).
				Msg("Successfully added service")
		}
	}

	log.Info().
		Int("added", successCount).
		Int("failed", failCount).
		Int("removed", len(toRemove)).
		Msg("Service reconciliation completed")

	if failCount > 0 {
		return fmt.Errorf("failed to add %d services", failCount)
	}

	// Reconcile funnel configuration (independent of serve)
	// Funnel and serve are separate features that can be used together or independently
	if err := c.reconcileFunnels(ctx, desiredServices); err != nil {
		log.Error().Err(err).Msg("Failed to reconcile funnel configurations")
		return fmt.Errorf("funnel reconciliation failed: %w", err)
	}

	// Sync Service Definitions to Control Plane (API)
	// This is done after local serve commands to ensure local state is consistent first,
	// but failures here are non-blocking for the local advertisement.
	if c.apiSyncEnabled {
		if err := c.syncServiceDefinitions(ctx, desiredServices); err != nil {
			// Log error but do NOT return it - we don't want API failures to break local serving
			log.Error().Err(err).Msg("Failed to sync service definitions to Tailscale API")
		}
	}

	return nil
}

// syncServiceDefinitions syncs all desired services to the Tailscale Control Plane
func (c *Client) syncServiceDefinitions(ctx context.Context, services []*apptypes.ContainerService) error {
	// Deduplicate by service name - we only need to upsert each service definition once
	// We also need to capture the port to send to the API
	type serviceDef struct {
		Tags []string
		Port string
	}
	uniqueServices := make(map[string]serviceDef)

	for _, svc := range services {
		// If multiple containers share a service name, we use the tags/port from the last one seen.
		// In a consistent config, they should be identical.
		// Note: svc.Port is the "service-port" (Tailscale side), not the container port.
		uniqueServices[svc.ServiceName] = serviceDef{
			Tags: svc.Tags,
			Port: svc.Port,
		}
	}

	log.Info().
		Int("unique_services", len(uniqueServices)).
		Msg("Syncing service definitions to Control Plane")

	var failed []string
	for name, def := range uniqueServices {
		if err := c.SyncServiceDefinition(ctx, name, def.Tags, def.Port); err != nil {
			failed = append(failed, name)
			log.Error().
				Err(err).
				Str("service", name).
				Msg("Failed to sync individual service definition")
			// Continue with others
		}
	}

	if len(failed) > 0 {
		return fmt.Errorf("failed to sync %d service(s) to Control Plane: %v", len(failed), failed)
	}

	return nil
}

// SyncServiceDefinition ensures a service definition exists in the Tailscale API.
// Only creates if the service doesn't exist. Does NOT update existing services.
func (c *Client) SyncServiceDefinition(ctx context.Context, serviceName string, tags []string, port string) error {
	if !strings.HasPrefix(serviceName, "svc:") {
		serviceName = "svc:" + serviceName
	}

	// Check if service already exists
	existing, err := c.getService(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service details: %w", err)
	}

	// If service already exists, skip creation
	if existing != nil {
		log.Debug().
			Str("service", serviceName).
			Strs("existing_tags", existing.Tags).
			Strs("existing_ports", existing.Ports).
			Msg("Service already exists in Control Plane, skipping creation")
		return nil
	}

	// Service doesn't exist, create it
	log.Info().
		Str("service", serviceName).
		Strs("tags", tags).
		Msg("Creating new service definition in Control Plane")

	apiURL := fmt.Sprintf("%s/api/v2/tailnet/%s/services/%s", c.baseURL, url.PathEscape(c.tailnet), url.PathEscape(serviceName))

	// Tailscale API requires "ports" to be present.
	if port == "" {
		port = "443"
	}

	// Tailscale API requires prefix for creation
	portStr := fmt.Sprintf("tcp:%s", port)

	payload := map[string]interface{}{
		"name":  serviceName,
		"tags":  tags,
		"ports": []string{portStr},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "PUT", apiURL, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	log.Debug().
		Str("method", "PUT").
		Str("url", apiURL).
		RawJSON("payload", body).
		Msg("Sending Control Plane request")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		log.Error().
			Int("status", resp.StatusCode).
			Str("body", string(respBody)).
			Msg("Control Plane request failed")
		return fmt.Errorf("API returned error status %d: %s", resp.StatusCode, string(respBody))
	}

	log.Info().
		Str("service", serviceName).
		Strs("tags", tags).
		Msg("Successfully created service definition in Control Plane")

	return nil
}

type apiService struct {
	Addrs []string `json:"addrs"`
	Tags  []string `json:"tags"`
	Ports []string `json:"ports"`
}

// getService fetches the existing service definition from the Tailscale API
// Returns nil if service does not exist (404)
func (c *Client) getService(ctx context.Context, serviceName string) (*apiService, error) {
	apiURL := fmt.Sprintf("%s/api/v2/tailnet/%s/services/%s", c.baseURL, url.PathEscape(c.tailnet), url.PathEscape(serviceName))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GET request: %w", err)
	}

	log.Debug().
		Str("method", "GET").
		Str("url", apiURL).
		Msg("Fetching existing service definition")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET API returned error status %d: %s", resp.StatusCode, string(body))
	}

	var svc apiService
	if err := json.NewDecoder(resp.Body).Decode(&svc); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &svc, nil
}

// CleanupAllServices removes all services and funnels managed by DockTail
// This is called on shutdown to ensure no orphaned services remain advertised
func (c *Client) CleanupAllServices(ctx context.Context) error {
	log.Info().Msg("Starting cleanup: removing all managed Tailscale services and funnels")

	var totalErrors []error

	// Cleanup funnels first (independent of services)
	currentFunnels, err := c.getCurrentFunnels(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get current funnels for cleanup, continuing with service cleanup")
	} else if len(currentFunnels) > 0 {
		log.Info().
			Int("funnel_count", len(currentFunnels)).
			Msg("Found funnels to clean up")

		for _, port := range currentFunnels {
			log.Info().
				Str("public_port", port).
				Msg("Cleaning up funnel")

			if err := c.removeFunnel(ctx, "cleanup", port); err != nil {
				log.Error().
					Err(err).
					Str("public_port", port).
					Msg("Failed to clean up funnel")
				totalErrors = append(totalErrors, err)
			}
		}
	}

	// Cleanup services
	currentServices, err := c.GetCurrentServices(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get current services for cleanup")
		return err
	}

	if len(currentServices) == 0 {
		log.Info().Msg("No services to clean up")
		if len(totalErrors) > 0 {
			return fmt.Errorf("cleanup completed with %d funnel errors", len(totalErrors))
		}
		return nil
	}

	log.Info().
		Int("service_count", len(currentServices)).
		Msg("Found services to clean up")

	// Remove each service (drain + clear)
	successCount := 0
	failCount := 0

	for _, svc := range currentServices {
		log.Info().
			Str("service", svc.ServiceName).
			Str("port", svc.Port).
			Str("protocol", svc.Protocol).
			Msg("Cleaning up service")

		if err := c.removeService(ctx, svc.ServiceName); err != nil {
			failCount++
			log.Error().
				Err(err).
				Str("service", svc.ServiceName).
				Msg("Failed to clean up service")
			totalErrors = append(totalErrors, err)
		} else {
			successCount++
		}
	}

	log.Info().
		Int("services_cleaned", successCount).
		Int("services_failed", failCount).
		Int("funnels_cleaned", len(currentFunnels)-len(totalErrors)).
		Int("total_errors", len(totalErrors)).
		Msg("Cleanup completed")

	if len(totalErrors) > 0 {
		return fmt.Errorf("cleanup completed with %d errors", len(totalErrors))
	}

	return nil
}
