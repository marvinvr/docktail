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
	socketPath           string
	tailnet              string
	baseURL              string
	httpClient           *http.Client
	apiSyncEnabled       bool
	serverVersion        string // set when CLI/daemon version mismatch detected
	managedFunnels       map[string]struct{}
	ignoredServices      map[string]struct{}
	deleteUnusedServices bool
}

// ClientConfig holds configuration for creating a Tailscale client
type ClientConfig struct {
	SocketPath         string
	Tailnet            string
	APIKey             string
	OAuthClientID      string
	OAuthClientSecret  string
	IgnoreServiceNames []string
	// DeleteUnusedServices enables removal of tailnet Service definitions that
	// are no longer advertised by any host during reconciliation.
	DeleteUnusedServices bool
}

// NewClient creates a new Tailscale client
// Prefers OAuth credentials over API key if both are provided
func NewClient(cfg ClientConfig) *Client {
	client := &Client{
		socketPath:           cfg.SocketPath,
		tailnet:              cfg.Tailnet,
		baseURL:              "https://api.tailscale.com",
		managedFunnels:       make(map[string]struct{}),
		ignoredServices:      make(map[string]struct{}),
		deleteUnusedServices: cfg.DeleteUnusedServices,
	}

	for _, serviceName := range cfg.IgnoreServiceNames {
		normalized := normalizeServiceName(serviceName)
		if normalized != "" {
			client.ignoredServices[normalized] = struct{}{}
		}
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
	// TCPForward is the backend address ("host:port") for plain-TCP service
	// endpoints. HTTP/HTTPS endpoints leave this empty and store their backend
	// in the Web handler config instead.
	TCPForward string `json:"TCPForward"`
}

type TailscaleWebConfig struct {
	Handlers map[string]TailscaleHandler `json:"Handlers"`
}

type TailscaleHandler struct {
	Proxy string `json:"Proxy"`
}

// ReconcileServices compares desired services with current services and makes necessary changes
func (c *Client) ReconcileServices(ctx context.Context, desiredServices []*apptypes.ContainerService) error {
	// Re-detect version mismatch each cycle in case tailscaled was updated
	c.DetectVersionMismatch(ctx)

	serviceDesiredCount := 0
	for _, svc := range desiredServices {
		if svc.ServiceEnabled {
			serviceDesiredCount++
		}
	}

	log.Info().
		Int("desired_count", serviceDesiredCount).
		Msg("Starting service reconciliation using CLI commands")

	// Build map of desired services for easy lookup
	desiredMap := make(map[string]*apptypes.ContainerService)
	for _, svc := range desiredServices {
		if !svc.ServiceEnabled {
			continue
		}
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
			if !sameDestination(current.Destination, expectedDest) || current.Protocol != desired.ServiceProtocol {
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
			if c.shouldIgnoreService(current.ServiceName) {
				log.Info().
					Str("service", current.ServiceName).
					Str("port", current.Port).
					Msg("Skipping removal for ignored service")
				continue
			}
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

		// Optionally delete tailnet Service definitions no host advertises anymore.
		// Runs after syncing so freshly created services are already in the desired
		// set and therefore excluded. Failures are non-blocking.
		if c.deleteUnusedServices {
			if err := c.deleteUnusedServiceDefinitions(ctx, desiredServices); err != nil {
				log.Error().Err(err).Msg("Failed to delete unused service definitions")
			}
		}
	}

	return nil
}

// serviceDef is the deduplicated, per-service-name view of the desired state
// that gets synced to the Control Plane.
type serviceDef struct {
	Tags        []string
	Ports       []string
	Description string
}

// aggregateServiceDefinitions deduplicates the desired services by name,
// aggregating all ports per service and carrying the tags and description.
// A container can define several services (primary + indexed) and several
// containers can back the same service name, so ports are merged and the first
// non-empty description wins.
func aggregateServiceDefinitions(services []*apptypes.ContainerService) map[string]*serviceDef {
	uniqueServices := make(map[string]*serviceDef)

	for _, svc := range services {
		if !svc.ServiceEnabled {
			continue
		}
		def, exists := uniqueServices[svc.ServiceName]
		if !exists {
			def = &serviceDef{Tags: svc.Tags, Description: svc.ServiceDescription}
			uniqueServices[svc.ServiceName] = def
		} else if def.Description == "" {
			def.Description = svc.ServiceDescription
		}
		// Add port if not already present
		found := false
		for _, p := range def.Ports {
			if p == svc.Port {
				found = true
				break
			}
		}
		if !found {
			def.Ports = append(def.Ports, svc.Port)
		}
	}

	return uniqueServices
}

// syncServiceDefinitions syncs all desired services to the Tailscale Control Plane
func (c *Client) syncServiceDefinitions(ctx context.Context, services []*apptypes.ContainerService) error {
	uniqueServices := aggregateServiceDefinitions(services)

	log.Info().
		Int("unique_services", len(uniqueServices)).
		Msg("Syncing service definitions to Control Plane")

	var failed []string
	for name, def := range uniqueServices {
		if err := c.SyncServiceDefinition(ctx, name, def.Tags, def.Ports, def.Description); err != nil {
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
// It creates the service (with an optional description) if it doesn't exist. For
// an already-existing service it does NOT touch tags or ports, but it does
// reconcile the description (the admin-panel "comment") when a description label
// is set and differs from the current value.
func (c *Client) SyncServiceDefinition(ctx context.Context, serviceName string, tags []string, ports []string, description string) error {
	if !strings.HasPrefix(serviceName, "svc:") {
		serviceName = "svc:" + serviceName
	}

	// Check if service already exists
	existing, err := c.getService(ctx, serviceName)
	if err != nil {
		return fmt.Errorf("failed to get service details: %w", err)
	}

	// If service already exists, only reconcile the description. Tags and ports
	// are intentionally left untouched to avoid clobbering manual changes.
	if existing != nil {
		if description == "" || existing.Comment == description {
			log.Debug().
				Str("service", serviceName).
				Strs("existing_tags", existing.Tags).
				Strs("existing_ports", existing.Ports).
				Msg("Service already exists in Control Plane, skipping creation")
			return nil
		}

		log.Info().
			Str("service", serviceName).
			Str("description", description).
			Msg("Updating service description in Control Plane")

		payload := map[string]interface{}{
			"name":    serviceName,
			"addrs":   existing.Addrs,
			"tags":    existing.Tags,
			"ports":   existing.Ports,
			"comment": description,
		}
		return c.putService(ctx, serviceName, payload)
	}

	// Service doesn't exist, create it
	log.Info().
		Str("service", serviceName).
		Strs("tags", tags).
		Str("description", description).
		Msg("Creating new service definition in Control Plane")

	// Tailscale API requires "ports" to be present.
	if len(ports) == 0 {
		ports = []string{"443"}
	}

	// Tailscale API requires "tcp:" prefix for ports
	portStrs := make([]string, len(ports))
	for i, p := range ports {
		portStrs[i] = fmt.Sprintf("tcp:%s", p)
	}

	payload := map[string]interface{}{
		"name":  serviceName,
		"tags":  tags,
		"ports": portStrs,
	}
	if description != "" {
		payload["comment"] = description
	}

	if err := c.putService(ctx, serviceName, payload); err != nil {
		return err
	}

	log.Info().
		Str("service", serviceName).
		Strs("tags", tags).
		Msg("Successfully created service definition in Control Plane")

	return nil
}

// putService PUTs a service definition payload to the Control Plane API.
func (c *Client) putService(ctx context.Context, serviceName string, payload map[string]interface{}) error {
	apiURL := fmt.Sprintf("%s/api/v2/tailnet/%s/services/%s", c.baseURL, url.PathEscape(c.tailnet), url.PathEscape(serviceName))

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

	return nil
}

type apiService struct {
	Name    string   `json:"name"`
	Addrs   []string `json:"addrs"`
	Tags    []string `json:"tags"`
	Ports   []string `json:"ports"`
	Comment string   `json:"comment"`
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

// listServices returns every Service definition configured in the tailnet.
func (c *Client) listServices(ctx context.Context) ([]apiService, error) {
	apiURL := fmt.Sprintf("%s/api/v2/tailnet/%s/services", c.baseURL, url.PathEscape(c.tailnet))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GET request: %w", err)
	}

	log.Debug().
		Str("method", "GET").
		Str("url", apiURL).
		Msg("Listing service definitions")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list services API returned error status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		VIPServices []apiService `json:"vipServices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.VIPServices, nil
}

// serviceHost describes a device that is advertising a Service, as returned by
// the "list devices hosting a Service" API.
type serviceHost struct {
	StableNodeID  string `json:"stableNodeID"`
	ApprovalLevel string `json:"approvalLevel"`
	Configured    string `json:"configured"`
}

// listServiceHosts returns the devices currently advertising the given Service.
// An empty slice means no host is advertising it, i.e. the Service is unused.
func (c *Client) listServiceHosts(ctx context.Context, serviceName string) ([]serviceHost, error) {
	apiURL := fmt.Sprintf("%s/api/v2/tailnet/%s/services/%s/devices", c.baseURL, url.PathEscape(c.tailnet), url.PathEscape(serviceName))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create GET request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list service hosts API returned error status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Hosts []serviceHost `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Hosts, nil
}

// deleteService removes a Service definition from the tailnet Control Plane.
// A 404 is treated as success since the goal (the Service no longer exists) is met.
func (c *Client) deleteService(ctx context.Context, serviceName string) error {
	apiURL := fmt.Sprintf("%s/api/v2/tailnet/%s/services/%s", c.baseURL, url.PathEscape(c.tailnet), url.PathEscape(serviceName))

	req, err := http.NewRequestWithContext(ctx, "DELETE", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create DELETE request: %w", err)
	}

	log.Debug().
		Str("method", "DELETE").
		Str("url", apiURL).
		Msg("Deleting service definition")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete service API returned error status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// deleteUnusedServiceDefinitions removes tailnet Service definitions that DockTail
// no longer advertises and that no other host is advertising either.
//
// It is deliberately conservative so it is safe to run from multiple DockTail
// instances against the same tailnet:
//   - Services DockTail currently wants (the desired set) are never touched. This
//     also protects a Service created earlier in this same reconcile cycle before
//     its advertising host has propagated to the Control Plane.
//   - Services listed in IGNORE_SERVICE_NAMES are never touched.
//   - A Service is deleted only when the Control Plane reports zero hosts advertising
//     it. A Service advertised by any other node/instance reports at least one host
//     and is left alone.
//   - Any API error while listing services or hosts aborts deletion for that Service;
//     DockTail never deletes under uncertainty.
func (c *Client) deleteUnusedServiceDefinitions(ctx context.Context, desiredServices []*apptypes.ContainerService) error {
	desired := make(map[string]struct{})
	for _, svc := range desiredServices {
		if !svc.ServiceEnabled {
			continue
		}
		desired[normalizeServiceName(svc.ServiceName)] = struct{}{}
	}

	services, err := c.listServices(ctx)
	if err != nil {
		return fmt.Errorf("failed to list services: %w", err)
	}

	log.Info().
		Int("service_count", len(services)).
		Msg("Checking tailnet for unused service definitions")

	deleted := 0
	var lastErr error

	for _, svc := range services {
		name := svc.Name
		if !isManagedService(name) {
			continue
		}
		if _, ok := desired[normalizeServiceName(name)]; ok {
			// DockTail advertises this service; keep it.
			continue
		}
		if c.shouldIgnoreService(name) {
			log.Debug().
				Str("service", name).
				Msg("Skipping unused-service cleanup for ignored service")
			continue
		}

		hosts, err := c.listServiceHosts(ctx, name)
		if err != nil {
			log.Warn().
				Err(err).
				Str("service", name).
				Msg("Failed to check service hosts, skipping deletion")
			lastErr = err
			continue
		}
		if len(hosts) > 0 {
			log.Debug().
				Str("service", name).
				Int("host_count", len(hosts)).
				Msg("Service still advertised by at least one host, keeping")
			continue
		}

		log.Info().
			Str("service", name).
			Msg("Deleting unused service definition (no advertising hosts)")

		if err := c.deleteService(ctx, name); err != nil {
			log.Error().
				Err(err).
				Str("service", name).
				Msg("Failed to delete unused service definition")
			lastErr = err
			continue
		}
		deleted++
	}

	if deleted > 0 {
		log.Info().
			Int("deleted", deleted).
			Msg("Deleted unused service definitions")
	}

	return lastErr
}

// CleanupAllServices removes all services and funnels managed by DockTail
// This is called on shutdown to ensure no orphaned services remain advertised
func (c *Client) CleanupAllServices(ctx context.Context) error {
	log.Info().Msg("Starting cleanup: removing all managed Tailscale services and funnels")

	var totalErrors []error
	funnelsCleaned := 0

	// Cleanup funnels first (independent of services)
	currentFunnels, err := c.getCurrentFunnels(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to get current funnels for cleanup, continuing with service cleanup")
	} else if len(currentFunnels) > 0 {
		log.Info().
			Int("funnel_count", len(currentFunnels)).
			Msg("Found funnels to clean up")

		ownedFunnels := make([]string, 0, len(currentFunnels))
		unmanagedFunnels := make([]string, 0)
		for publicPort := range currentFunnels {
			if _, managed := c.managedFunnels[publicPort]; managed {
				ownedFunnels = append(ownedFunnels, publicPort)
			} else {
				unmanagedFunnels = append(unmanagedFunnels, publicPort)
			}
		}

		if len(ownedFunnels) == 0 {
			log.Info().Msg("Skipping funnel cleanup: no current funnels are known to be managed by this DockTail process")
		} else if len(unmanagedFunnels) > 0 {
			log.Warn().
				Strs("managed_public_ports", ownedFunnels).
				Strs("unmanaged_public_ports", unmanagedFunnels).
				Msg("Skipping funnel cleanup because unmanaged funnels exist on this node")
		} else {
			if err := c.resetFunnels(ctx, "cleanup"); err != nil {
				log.Error().Err(err).Msg("Failed to clean up funnels")
				totalErrors = append(totalErrors, err)
			} else {
				funnelsCleaned = len(currentFunnels)
				c.managedFunnels = make(map[string]struct{})
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
		if c.shouldIgnoreService(svc.ServiceName) {
			log.Info().
				Str("service", svc.ServiceName).
				Str("port", svc.Port).
				Msg("Skipping cleanup for ignored service")
			continue
		}

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
		Int("funnels_cleaned", funnelsCleaned).
		Int("total_errors", len(totalErrors)).
		Msg("Cleanup completed")

	if len(totalErrors) > 0 {
		return fmt.Errorf("cleanup completed with %d errors", len(totalErrors))
	}

	return nil
}
