package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// HTTP-to-HTTPS redirects (docktail.service.http-redirect).
//
// Tailscale Serve can answer an HTTP handler with a redirect (the HTTPHandler
// "Redirect" field, tailscaled 1.92+), but the tailscale CLI has no flag that
// writes one. DockTail therefore writes this one endpoint through tailscaled's
// LocalAPI serve-config endpoint, the same read-modify-write the CLI does
// internally, guarded by the config's ETag. Every other endpoint is still
// configured through the CLI, and the CLI preserves the redirect handler when
// it edits the rest of the service, because it round-trips the full config.

const (
	// httpRedirectPort is where the redirect listens: the port a browser uses
	// for a bare http:// URL.
	httpRedirectPort = "80"
	// httpRedirectCode is a permanent redirect that, unlike 301, keeps the
	// request method and body.
	httpRedirectCode = "308"
	// redirectMinMajor/Minor is the first tailscaled release whose Serve config
	// knows the Redirect handler. Older daemons silently drop the field and
	// would serve an empty handler instead.
	redirectMinMajor = 1
	redirectMinMinor = 92

	localAPIHost        = "local-tailscaled.sock"
	defaultSocketPath   = "/var/run/tailscale/tailscaled.sock"
	serveConfigEndpoint = "http://" + localAPIHost + "/localapi/v0/serve-config"
	// serveConfigAttempts bounds retries when another writer (the CLI, a
	// human) changes the Serve config between our read and write.
	serveConfigAttempts = 3
)

// redirectTarget is the Serve redirect value for a service's HTTPS endpoint.
// It names the full MagicDNS FQDN instead of Tailscale's ${HOST} placeholder:
// ${HOST} repeats whatever Host the client sent, so http://web/ would redirect
// to https://web/, a name the service certificate does not cover
// (tailscale/tailscale#21683).
func redirectTarget(serviceName, httpsPort, magicDNSSuffix string) string {
	host := serviceName + "." + magicDNSSuffix
	if httpsPort != "" && httpsPort != "443" {
		host += ":" + httpsPort
	}
	return httpRedirectCode + ":https://" + host + "${REQUEST_URI}"
}

// redirectWebKey is the Serve Web key tailscaled looks up for an HTTP request
// to a Service: "<name>.<MagicDNS suffix>:<port>". Service names are used as
// the labels declare them, exactly like the CLI-configured endpoints.
func redirectWebKey(serviceName, port, magicDNSSuffix string) string {
	return serviceName + "." + magicDNSSuffix + ":" + port
}

// expandHTTPRedirects returns desired plus one derived port-80 endpoint per
// enabled service that asked for an HTTP redirect. Deriving them as ordinary
// endpoints keeps everything downstream unchanged: conflict detection (a
// container serving port 80 on the same name conflicts), Service definition
// port sync (new definitions get tcp:80) and stale-endpoint removal.
func expandHTTPRedirects(desired []*apptypes.ContainerService, magicDNSSuffix string) []*apptypes.ContainerService {
	out := desired
	for _, svc := range desired {
		if !svc.ServiceEnabled || !svc.HTTPRedirect || svc.ServiceProtocol != "https" {
			continue
		}
		if len(out) == len(desired) {
			out = append([]*apptypes.ContainerService(nil), desired...)
		}
		out = append(out, &apptypes.ContainerService{
			ContainerID:        svc.ContainerID,
			ContainerName:      svc.ContainerName,
			ServiceEnabled:     true,
			ServiceName:        svc.ServiceName,
			ServiceDescription: svc.ServiceDescription,
			Port:               httpRedirectPort,
			ServiceProtocol:    "http",
			ServicePath:        "/",
			Tags:               svc.Tags,
			RedirectTo:         redirectTarget(svc.ServiceName, svc.Port, magicDNSSuffix),
		})
	}
	return out
}

func wantsHTTPRedirect(desired []*apptypes.ContainerService) bool {
	return len(httpRedirectServiceNames(desired)) > 0
}

// httpRedirectServiceNames returns the "svc:"-prefixed names of the enabled
// services that ask for an HTTP redirect.
func httpRedirectServiceNames(desired []*apptypes.ContainerService) map[string]struct{} {
	names := make(map[string]struct{})
	for _, svc := range desired {
		if svc.ServiceEnabled && svc.HTTPRedirect && svc.ServiceProtocol == "https" {
			names["svc:"+svc.ServiceName] = struct{}{}
		}
	}
	return names
}

// withHTTPRedirects adds the derived redirect endpoints when any service asks
// for one. Without a MagicDNS suffix, or on a tailscaled too old to serve a
// redirect, it logs why and leaves them out, so the HTTPS endpoints themselves
// are never held back.
// The MagicDNS suffix is returned for writing the redirect handlers.
func (c *Client) withHTTPRedirects(ctx context.Context, desired []*apptypes.ContainerService) ([]*apptypes.ContainerService, string) {
	if !wantsHTTPRedirect(desired) {
		return desired, ""
	}
	st, err := c.Status(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("HTTP redirects skipped this cycle: could not read the tailnet's MagicDNS name")
		return desired, ""
	}
	if st.MagicDNSSuffix == "" {
		log.Warn().Msg("HTTP redirects skipped: tailscaled reports no MagicDNS suffix; enable MagicDNS and HTTPS certificates for the tailnet")
		return desired, ""
	}
	if !supportsServeRedirect(st.Version) {
		log.Warn().
			Str("tailscaled_version", st.Version).
			Msgf("HTTP redirects skipped: tailscaled %s does not support Serve redirects; update it to %d.%d or newer", st.Version, redirectMinMajor, redirectMinMinor)
		return desired, ""
	}
	return expandHTTPRedirects(desired, st.MagicDNSSuffix), st.MagicDNSSuffix
}

// supportsServeRedirect reports whether a tailscaled version ("1.104.4-t...")
// knows the Serve Redirect handler. Unparseable versions (development builds)
// are assumed to support it rather than silently disabling the feature.
func supportsServeRedirect(version string) bool {
	parts := strings.SplitN(strings.TrimSpace(version), ".", 3)
	if len(parts) < 2 {
		return true
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(strings.SplitN(parts[1], "-", 2)[0])
	if err1 != nil || err2 != nil {
		return true
	}
	if major != redirectMinMajor {
		return major > redirectMinMajor
	}
	return minor >= redirectMinMinor
}

// redirectConfigChanged compares a served endpoint with a derived redirect.
func redirectConfigChanged(current ServiceEndpoint, desired *apptypes.ContainerService) bool {
	return current.Protocol != "http" ||
		current.Redirect != desired.RedirectTo ||
		current.Destination != "" ||
		normalizeServicePath(current.Path) != "/"
}

// setHTTPRedirect makes port 80 of the service answer every request with the
// redirect, replacing whatever handlers that port had.
func (c *Client) setHTTPRedirect(ctx context.Context, svc *apptypes.ContainerService, magicDNSSuffix string) error {
	serviceName := "svc:" + svc.ServiceName
	webKey := redirectWebKey(svc.ServiceName, svc.Port, magicDNSSuffix)
	return c.updateServeConfig(ctx, func(cfg map[string]any) bool {
		services := childMap(cfg, "Services")
		service := childMap(services, serviceName)
		childMap(service, "TCP")[svc.Port] = map[string]any{"HTTP": true}
		childMap(service, "Web")[webKey] = map[string]any{
			"Handlers": map[string]any{"/": map[string]any{"Redirect": svc.RedirectTo}},
		}
		return true
	})
}

// removeHTTPRedirect removes only the redirect port of a service and reports
// whether the service is left without any port, in which case the caller
// removes (drains and clears) the whole service instead.
func (c *Client) removeHTTPRedirect(ctx context.Context, current ServiceEndpoint) (serviceEmpty bool, err error) {
	err = c.updateServeConfig(ctx, func(cfg map[string]any) bool {
		services, _ := cfg["Services"].(map[string]any)
		service, _ := services[current.ServiceName].(map[string]any)
		if service == nil {
			return false
		}
		changed := false
		if tcp, _ := service["TCP"].(map[string]any); tcp != nil {
			if _, ok := tcp[current.Port]; ok {
				delete(tcp, current.Port)
				changed = true
			}
			serviceEmpty = len(tcp) == 0
		} else {
			serviceEmpty = true
		}
		if web, _ := service["Web"].(map[string]any); web != nil {
			for key := range web {
				if strings.HasSuffix(key, ":"+current.Port) {
					delete(web, key)
					changed = true
				}
			}
		}
		return changed
	})
	return serviceEmpty, err
}

// childMap returns m[key] as a map, creating it when missing or null.
func childMap(m map[string]any, key string) map[string]any {
	if child, ok := m[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	m[key] = child
	return child
}

// updateServeConfig reads tailscaled's Serve config, applies mutate and writes
// it back with If-Match, retrying when someone else wrote in between. The
// config is handled as generic JSON so fields DockTail does not model (Funnel,
// node-level handlers, future additions) are written back unchanged.
func (c *Client) updateServeConfig(ctx context.Context, mutate func(cfg map[string]any) bool) error {
	httpClient := c.localAPIClient()
	for attempt := 1; ; attempt++ {
		cfg, etag, err := getServeConfig(ctx, httpClient)
		if err != nil {
			return err
		}
		if !mutate(cfg) {
			return nil
		}
		err = postServeConfig(ctx, httpClient, cfg, etag)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errServeConfigChanged) || attempt >= serveConfigAttempts {
			return err
		}
		log.Debug().Int("attempt", attempt).Msg("Serve config changed concurrently, retrying redirect update")
	}
}

var errServeConfigChanged = errors.New("serve config changed concurrently")

func getServeConfig(ctx context.Context, httpClient *http.Client) (map[string]any, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serveConfigEndpoint, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("read serve config: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read serve config: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", localAPIError("read serve config", resp.StatusCode, body)
	}
	cfg := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep numbers exactly as tailscaled sent them
	var decoded map[string]any
	if err := dec.Decode(&decoded); err != nil {
		return nil, "", fmt.Errorf("parse serve config: %w", err)
	}
	if decoded != nil { // "null" when nothing is served yet
		cfg = decoded
	}
	return cfg, resp.Header.Get("Etag"), nil
}

func postServeConfig(ctx context.Context, httpClient *http.Client, cfg map[string]any, etag string) error {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode serve config: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serveConfigEndpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("write serve config: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusPreconditionFailed:
		return errServeConfigChanged
	default:
		return localAPIError("write serve config", resp.StatusCode, body)
	}
}

func localAPIError(op string, status int, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if isServeConfigDeniedError(msg) {
		return errors.New(serveConfigDeniedMessage("configure the HTTP redirect"))
	}
	return fmt.Errorf("%s: tailscaled LocalAPI returned %d: %s", op, status, msg)
}

// localAPIClient talks HTTP to tailscaled over its Unix socket, as the
// tailscale CLI does.
func (c *Client) localAPIClient() *http.Client {
	socket := c.socketPath
	if socket == "" {
		socket = defaultSocketPath
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}
