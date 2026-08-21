package docker

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// indexedPortRegex matches labels like "docktail.service.1.port", "docktail.service.2.port", etc.
// Each indexed entry defines a separate Tailscale service (requires docktail.service.N.name).
var indexedPortRegex = regexp.MustCompile(`^docktail\.service\.(\d+)\.port$`)

// containerCtx holds shared container context used across multi-port parsing.
type containerCtx struct {
	containerID      string
	containerName    string
	specifiedNetwork string
	inspect          container.InspectResponse
	tags             []string
	destIP           string
	isHostNetwork    bool
	isNoNetwork      bool
	isDirectMode     bool
}

// Client wraps the Docker client with our business logic
type Client struct {
	cli         *client.Client
	defaultTags []string

	// gatewayIP caches the address used to reach host-networked services from
	// inside the agent's own container (see hostGatewayIP). Resolved at most once.
	gatewayOnce sync.Once
	gatewayIP   string

	// selfNetIDs caches the docker network IDs the agent's own container is
	// attached to, used to decide whether a target container is reachable at its
	// own network IP (see sharesNetworkWith). Resolved at most once.
	selfNetOnce sync.Once
	selfNetIDs  map[string]struct{}
}

// NewClient creates a new Docker client
func NewClient(defaultTags []string) (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &Client{cli: cli, defaultTags: defaultTags}, nil
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
			// Widened for the optional cloud module's failure signals. These do
			// not trigger a tailscale reconcile (see reconciler.triggersReconcile),
			// they are only forwarded to the cloud observer.
			filters.Arg("event", "oom"),
			filters.Arg("event", "health_status"),
		),
	})

	return eventsChan, errChan
}

func isServiceEnabled(labels map[string]string) bool {
	return labels[apptypes.LabelEnable] == "true"
}

func isFunnelEnabled(labels map[string]string) bool {
	return labels[apptypes.LabelFunnelEnable] == "true"
}

func isManagedContainer(labels map[string]string) bool {
	return isServiceEnabled(labels) || isFunnelEnabled(labels)
}

// GetEnabledContainers returns all running containers managed by DockTail.
// A container can be managed by a Tailscale service, a funnel, or both.
func (c *Client) GetEnabledContainers(ctx context.Context) ([]*apptypes.ContainerService, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var services []*apptypes.ContainerService
	for _, cont := range containers {
		if !isManagedContainer(cont.Labels) {
			continue
		}

		parsed, err := c.parseContainer(ctx, cont.ID, cont.Labels)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container_id", cont.ID[:12]).
				Str("container_name", strings.TrimPrefix(cont.Names[0], "/")).
				Msg("Failed to parse container, skipping")
			continue
		}
		services = append(services, parsed...)
	}

	return services, nil
}

// resolveProtocols applies smart defaults for container protocol, service port, and service protocol.
// Returns (protocol, servicePort, serviceProtocol, error).
func resolveProtocols(containerID, targetPort, servicePort, serviceProtocol, protocol string) (string, string, string, error) {
	// Smart defaults for target/container protocol based on CONTAINER port
	if protocol == "" {
		switch targetPort {
		case "443":
			protocol = "https"
		default:
			protocol = "http"
		}
		log.Debug().
			Str("container", containerID[:12]).
			Str("container_port", targetPort).
			Str("defaulted_protocol", protocol).
			Msg("Container protocol not specified, defaulted based on container port")
	}

	// Validate target protocol
	validProtocols := map[string]bool{
		"http":               true,
		"https":              true,
		"https+insecure":     true,
		"tcp":                true,
		"tls-terminated-tcp": true,
	}
	if !validProtocols[protocol] {
		return "", "", "", fmt.Errorf("invalid protocol: %s (must be http, https, https+insecure, tcp, or tls-terminated-tcp)", protocol)
	}

	// Smart defaults based on both fields
	if servicePort == "" && serviceProtocol == "" {
		if protocol == "tcp" || protocol == "tls-terminated-tcp" {
			servicePort = "80"
			serviceProtocol = protocol
			log.Debug().
				Str("container", containerID[:12]).
				Str("backend_protocol", protocol).
				Msg("No port or service protocol specified, defaulting to TCP on port 80 to match backend")
		} else {
			servicePort = "80"
			serviceProtocol = "http"
			log.Debug().
				Str("container", containerID[:12]).
				Msg("No port or protocol specified, defaulting to HTTP on port 80")
		}
	} else if servicePort == "" && serviceProtocol != "" {
		switch serviceProtocol {
		case "https":
			servicePort = "443"
		default:
			servicePort = "80"
		}
		log.Debug().
			Str("container", containerID[:12]).
			Str("service_protocol", serviceProtocol).
			Str("defaulted_service_port", servicePort).
			Msg("Service port not specified, defaulted based on protocol")
	} else if servicePort != "" && serviceProtocol == "" {
		if protocol == "tcp" || protocol == "tls-terminated-tcp" {
			serviceProtocol = protocol
			log.Debug().
				Str("container", containerID[:12]).
				Str("service_port", servicePort).
				Str("backend_protocol", protocol).
				Str("defaulted_service_protocol", serviceProtocol).
				Msg("Service protocol not specified, defaulted to match backend TCP protocol")
		} else {
			switch servicePort {
			case "443":
				serviceProtocol = "https"
			case "80":
				serviceProtocol = "http"
			default:
				serviceProtocol = "http"
			}
			log.Debug().
				Str("container", containerID[:12]).
				Str("service_port", servicePort).
				Str("defaulted_service_protocol", serviceProtocol).
				Msg("Service protocol not specified, defaulted based on port")
		}
	}

	// Validate service protocol
	validServiceProtocols := map[string]bool{
		"http":               true,
		"https":              true,
		"tcp":                true,
		"tls-terminated-tcp": true,
	}
	if !validServiceProtocols[serviceProtocol] {
		return "", "", "", fmt.Errorf("invalid service-protocol: %s (must be http, https, tcp, or tls-terminated-tcp)", serviceProtocol)
	}

	return protocol, servicePort, serviceProtocol, nil
}

// parseProxyProtocol validates docktail.service.proxy-protocol. Empty means
// disabled (0). Tailscale only accepts versions 1 or 2, and only on TCP
// forwarding (service-protocol tcp or tls-terminated-tcp). HTTP/HTTPS must
// reject the label: Tailscale errors with "PROXY protocol is only supported
// for TCP forwarding", which would fail every reconcile.
func parseProxyProtocol(value, serviceProtocol string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if serviceProtocol != "tcp" && serviceProtocol != "tls-terminated-tcp" {
		return 0, fmt.Errorf("docktail.service.proxy-protocol is only supported for TCP forwarding (service-protocol tcp or tls-terminated-tcp), not %s", serviceProtocol)
	}
	switch value {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, fmt.Errorf("invalid docktail.service.proxy-protocol: %q (must be 1 or 2)", value)
	}
}

// resolveDestPort determines the destination IP and port based on networking mode.
// Returns (destIP, destPort, error).
func (c *Client) resolveDestPort(cctx *containerCtx, targetPort string) (string, string, error) {
	if cctx.isHostNetwork {
		log.Info().
			Str("container", cctx.containerName).
			Str("port", targetPort).
			Msg("Container uses host networking, port is directly accessible on 127.0.0.1")
		// Use 127.0.0.1 (not "localhost") to force IPv4: on dual-stack hosts
		// "localhost" resolves to ::1 first, but the port is typically bound on
		// IPv4 only, so the IPv6 attempt is refused.
		return "127.0.0.1", targetPort, nil
	}

	if cctx.isDirectMode {
		if cctx.isNoNetwork {
			return "", "", fmt.Errorf("container '%s' uses network_mode: none, cannot use direct mode", cctx.containerName)
		}

		containerIP, networkName, err := c.getContainerIP(cctx.inspect, cctx.specifiedNetwork, cctx.containerName)
		if err != nil {
			return "", "", err
		}

		if err := c.checkReachability(containerIP, targetPort); err != nil {
			log.Debug().
				Str("container", cctx.containerName).
				Str("container_ip", containerIP).
				Str("port", targetPort).
				Msg("Container not yet reachable (may still be starting)")
		}

		log.Info().
			Str("container", cctx.containerName).
			Str("container_ip", containerIP).
			Str("container_port", targetPort).
			Str("network", networkName).
			Str("will_proxy_to", fmt.Sprintf("%s:%s", containerIP, targetPort)).
			Msg("Proxying directly to container IP (no port publishing required)")

		return containerIP, targetPort, nil
	}

	// Published port mode
	targetPortKey := nat.Port(fmt.Sprintf("%s/tcp", targetPort))
	var hostPort string

	log.Debug().
		Str("container", cctx.containerName).
		Str("looking_for_port", string(targetPortKey)).
		Msg("Direct mode disabled, looking for published port binding")

	if cctx.inspect.HostConfig != nil && cctx.inspect.HostConfig.PortBindings != nil {
		if bindings, ok := cctx.inspect.HostConfig.PortBindings[targetPortKey]; ok && len(bindings) > 0 {
			hostPort = bindings[0].HostPort
			log.Debug().
				Str("container", cctx.containerName).
				Str("target_port", targetPort).
				Str("host_port", hostPort).
				Msg("Detected published port binding")
		}
	}

	if hostPort == "" && cctx.inspect.NetworkSettings != nil && cctx.inspect.NetworkSettings.Ports != nil {
		if bindings, ok := cctx.inspect.NetworkSettings.Ports[targetPortKey]; ok && len(bindings) > 0 {
			hostPort = bindings[0].HostPort
			log.Debug().
				Str("container", cctx.containerName).
				Str("target_port", targetPort).
				Str("host_port", hostPort).
				Msg("Detected published port from NetworkSettings")
		}
	}

	if hostPort == "" {
		var availablePorts []string
		if cctx.inspect.HostConfig != nil && cctx.inspect.HostConfig.PortBindings != nil {
			for port := range cctx.inspect.HostConfig.PortBindings {
				availablePorts = append(availablePorts, string(port))
			}
		}

		log.Warn().
			Str("container", cctx.containerName).
			Str("needed_port", string(targetPortKey)).
			Strs("available_ports", availablePorts).
			Msg("Port not found in bindings (direct mode is disabled)")

		return "", "", fmt.Errorf(
			"container port %s is NOT published to host (direct mode disabled via docktail.service.direct=false). "+
				"Fix: Add 'ports: [\"%s:%s\"]' to container '%s' in docker-compose.yaml, "+
				"or remove 'docktail.service.direct=false' to use container IP directly. "+
				"Available published ports: %v",
			targetPort, targetPort, targetPort, cctx.containerName, availablePorts,
		)
	}

	log.Info().
		Str("container", cctx.containerName).
		Str("container_port", targetPort).
		Str("host_port", hostPort).
		Str("will_proxy_to", fmt.Sprintf("127.0.0.1:%s", hostPort)).
		Msg("Direct mode disabled - using published port binding")

	// Use 127.0.0.1 (not "localhost") to force IPv4: Docker publishes ports on
	// IPv4 (0.0.0.0) by default, but on dual-stack hosts "localhost" resolves to
	// ::1 first, so the IPv6 connection attempt is refused.
	return "127.0.0.1", hostPort, nil
}

// monitorTarget returns an explicit address for the LOCAL health check to probe
// the container directly, or ("", "") to fall back to the serve destination
// (IPAddress:TargetPort). The serve destination is correct for tailscaled (which
// runs in the host's network namespace), but the health check runs inside the
// agent's own container, so any host-relative 127.0.0.1 address is wrong for it.
// containerPort is the container's own port (the docktail.service[.N].port label).
// This does NOT affect tailscale serve, which keeps using IPAddress/TargetPort.
func (c *Client) monitorTarget(ctx context.Context, cctx *containerCtx, containerPort string) (string, string) {
	if cctx.isNoNetwork {
		// No address to probe.
		return "", ""
	}
	if cctx.isHostNetwork {
		// Checked before isDirectMode: a host-network container is "direct" by
		// default (no docktail.service.direct=false label), but its serve
		// destination is 127.0.0.1:<port> — correct for tailscaled (host netns) yet
		// the agent's own container loopback otherwise. Probe the host via its
		// docker-network gateway instead. Empty ⇒ the agent itself shares the host
		// netns, so 127.0.0.1 is already correct: fall back to it.
		gw := c.hostGatewayIP(ctx)
		if gw == "" {
			return "", ""
		}
		return gw, containerPort
	}

	// Direct/published-port modes target the container's own network IP, which the
	// agent can reach only if it shares a docker network with the container. When
	// it doesn't — e.g. a service isolated behind a VPN sidecar (qbittorrent via
	// gluetun) on its own compose network — fall back to the container's published
	// host port reached via the agent's docker-network gateway. An empty gateway
	// means the agent shares the host netns and already reaches container IPs, so
	// only override for a containerised agent.
	if !c.sharesNetworkWith(ctx, cctx.inspect) {
		if gw := c.hostGatewayIP(ctx); gw != "" {
			if hp := publishedHostPort(cctx.inspect, containerPort); hp != "" {
				return gw, hp
			}
		}
	}

	switch {
	case cctx.isDirectMode:
		// IPAddress/TargetPort already point at the container's own network IP,
		// reachable from the agent's container; no override needed.
		return "", ""
	default:
		// Published-port mode: the serve destination is a host-relative
		// 127.0.0.1:<hostPort> the agent can't reach from inside its container, so
		// probe the container's own network IP and container port instead.
		ip, _, err := c.getContainerIP(cctx.inspect, cctx.specifiedNetwork, cctx.containerName)
		if err != nil || ip == "" {
			return "", ""
		}
		return ip, containerPort
	}
}

// ensureSelfNetIDs populates, at most once, the set of docker network IDs the
// agent's own container is attached to. It is best-effort: the set stays empty
// when the agent isn't a resolvable container (bare-metal / host netns), in
// which case callers fall back to network-agnostic behaviour.
func (c *Client) ensureSelfNetIDs(ctx context.Context) {
	c.selfNetOnce.Do(func() {
		inspect, ok := c.inspectSelf(ctx, ownContainerID())
		if !ok || inspect.NetworkSettings == nil {
			return
		}
		c.selfNetIDs = make(map[string]struct{}, len(inspect.NetworkSettings.Networks))
		for _, n := range inspect.NetworkSettings.Networks {
			if n.NetworkID != "" {
				c.selfNetIDs[n.NetworkID] = struct{}{}
			}
		}
	})
}

// sharesNetworkWith reports whether the agent's own container is attached to any
// of the same docker networks as target, i.e. whether the agent can reach
// target at its own network IP. Network IDs the agent is attached to are
// resolved once and cached. Returns false when the agent isn't a resolvable
// container (bare-metal / host netns) — callers treat that case via the gateway.
func (c *Client) sharesNetworkWith(ctx context.Context, target container.InspectResponse) bool {
	c.ensureSelfNetIDs(ctx)
	if len(c.selfNetIDs) == 0 || target.NetworkSettings == nil {
		return false
	}
	for _, n := range target.NetworkSettings.Networks {
		if n.NetworkID == "" {
			continue
		}
		if _, ok := c.selfNetIDs[n.NetworkID]; ok {
			return true
		}
	}
	return false
}

// publishedHostPort returns the host port a container publishes for the given
// container port (tcp), or "" if the port isn't published. This is the address
// reachable on the host (and thus via the docker host gateway) regardless of
// which docker network the container sits on.
func publishedHostPort(inspect container.InspectResponse, containerPort string) string {
	key := nat.Port(fmt.Sprintf("%s/tcp", containerPort))
	if inspect.HostConfig != nil && inspect.HostConfig.PortBindings != nil {
		if b, ok := inspect.HostConfig.PortBindings[key]; ok && len(b) > 0 && b[0].HostPort != "" {
			return b[0].HostPort
		}
	}
	if inspect.NetworkSettings != nil && inspect.NetworkSettings.Ports != nil {
		if b, ok := inspect.NetworkSettings.Ports[key]; ok && len(b) > 0 && b[0].HostPort != "" {
			return b[0].HostPort
		}
	}
	return ""
}

// hostGatewayIP returns an address the agent can use to reach services running in
// the host's network namespace (network_mode: host). The agent runs in its own
// container, so the host is reachable via the gateway of one of the agent's docker
// networks — the host's address on that bridge, where a host-networked service
// bound to 0.0.0.0 is reachable. Resolved at most once and cached.
//
// Returns "" when the agent itself shares the host netns (127.0.0.1 already works),
// or "host.docker.internal" as a best-effort fallback when the gateway can't be
// resolved (Docker's host alias; needs extra_hosts host.docker.internal:host-gateway
// on Linux).
func (c *Client) hostGatewayIP(ctx context.Context) string {
	c.gatewayOnce.Do(func() {
		c.gatewayIP = c.resolveHostGateway(ctx)
	})
	return c.gatewayIP
}

// selfContainerMountRegex extracts the agent's own container ID from a
// /var/lib/docker/containers/<id>/ mount source in /proc/self/mountinfo (Docker
// bind-mounts /etc/hostname, /etc/hosts and /etc/resolv.conf from there).
var selfContainerMountRegex = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// hex64Regex matches a full Docker container ID anywhere (e.g. in a cgroup path).
var hex64Regex = regexp.MustCompile(`[0-9a-f]{64}`)

// ownContainerID best-effort discovers the agent's own container ID from /proc so
// the agent can inspect itself even when its hostname doesn't match the container
// (a custom hostname:, or sharing the host's UTS namespace). Returns "" when the
// agent is not running inside a container (e.g. a bare-metal binary).
func ownContainerID() string {
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if m := selfContainerMountRegex.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if id := hex64Regex.Find(data); id != nil {
			return string(id)
		}
	}
	return ""
}

// inspectSelf returns the InspectResponse for the agent's own container, trying
// the /proc-discovered container ID first (robust against custom hostnames and
// the host UTS namespace) and then the hostname. ok is false when neither
// reference resolves (e.g. a bare-metal binary).
func (c *Client) inspectSelf(ctx context.Context, id string) (container.InspectResponse, bool) {
	refs := make([]string, 0, 2)
	if id != "" {
		refs = append(refs, id)
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		refs = append(refs, host)
	}
	for _, ref := range refs {
		inspect, err := c.cli.ContainerInspect(ctx, ref)
		if err == nil {
			return inspect, true
		}
		log.Debug().Err(err).Str("ref", ref).
			Msg("Could not inspect agent's own container by this reference")
	}
	return container.InspectResponse{}, false
}

func (c *Client) resolveHostGateway(ctx context.Context) string {
	const hostAlias = "host.docker.internal"

	id := ownContainerID()
	inspect, ok := c.inspectSelf(ctx, id)
	if !ok {
		// No /proc container ID and no inspectable container ⇒ the agent runs
		// directly on the host (bare-metal) or shares the host netns, where
		// 127.0.0.1 already reaches host-networked services. Empty ⇒ caller falls
		// back to 127.0.0.1.
		if id == "" {
			return ""
		}
		// We *are* in a container but couldn't inspect it (unexpected): fall back to
		// Docker's host alias (needs extra_hosts host.docker.internal:host-gateway).
		log.Debug().Str("id", id).
			Msg("Could not inspect agent's own container to resolve host gateway; falling back to host.docker.internal")
		return hostAlias
	}

	// Agent on host networking: 127.0.0.1 already reaches host-networked services.
	if inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "host" {
		return ""
	}
	if inspect.NetworkSettings == nil {
		return hostAlias
	}

	// Any docker bridge gateway routes to the host; prefer "bridge", else pick
	// deterministically for stable logging.
	networks := inspect.NetworkSettings.Networks
	if n, ok := networks["bridge"]; ok && n.Gateway != "" {
		log.Debug().Str("network", "bridge").Str("gateway", n.Gateway).
			Msg("Resolved host gateway for host-networked service probes")
		return n.Gateway
	}
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if gw := networks[name].Gateway; gw != "" {
			log.Debug().Str("network", name).Str("gateway", gw).
				Msg("Resolved host gateway for host-networked service probes")
			return gw
		}
	}
	return hostAlias
}

type funnelConfig struct {
	IPAddress  string
	Port       string
	TargetPort string
	PublicPort string
	Protocol   string
	Path       string
}

func parseFunnelPath(rawPath string) (string, error) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "/", nil
	}
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("invalid funnel path: %s (must start with /)", rawPath)
	}
	return path, nil
}

func (c *Client) parseFunnelConfig(cctx *containerCtx, labels map[string]string) (*funnelConfig, error) {
	if !isFunnelEnabled(labels) {
		return nil, nil
	}

	funnelPort := labels[apptypes.LabelFunnelPort]
	if funnelPort == "" {
		return nil, fmt.Errorf("funnel enabled but missing required label: %s (container port)", apptypes.LabelFunnelPort)
	}

	funnelProtocol := labels[apptypes.LabelFunnelProtocol]
	if funnelProtocol == "" {
		funnelProtocol = "https"
		log.Debug().
			Str("container", cctx.containerID[:12]).
			Msg("Funnel protocol not specified, defaulting to HTTPS")
	}

	funnelFunnelPort := labels[apptypes.LabelFunnelFunnelPort]
	if funnelFunnelPort == "" {
		funnelFunnelPort = "443"
		log.Debug().
			Str("container", cctx.containerID[:12]).
			Msg("Funnel public port not specified, defaulting to 443")
	}

	if funnelProtocol == "https" || funnelProtocol == "http" {
		validFunnelPorts := map[string]bool{"443": true, "8443": true, "10000": true}
		if !validFunnelPorts[funnelFunnelPort] {
			return nil, fmt.Errorf("invalid funnel-port: %s for HTTPS/HTTP (must be 443, 8443, or 10000)", funnelFunnelPort)
		}
	}

	validFunnelProtocols := map[string]bool{"http": true, "https": true, "tcp": true, "tls-terminated-tcp": true}
	if !validFunnelProtocols[funnelProtocol] {
		return nil, fmt.Errorf("invalid funnel protocol: %s (must be http, https, tcp, or tls-terminated-tcp)", funnelProtocol)
	}

	funnelPathLabel, hasFunnelPath := labels[apptypes.LabelFunnelPath]
	if hasFunnelPath && (funnelProtocol == "tcp" || funnelProtocol == "tls-terminated-tcp") {
		return nil, fmt.Errorf("%s is only supported for HTTP/HTTPS funnels", apptypes.LabelFunnelPath)
	}
	funnelPath, err := parseFunnelPath(funnelPathLabel)
	if err != nil {
		return nil, err
	}

	funnelDestIP, funnelTargetPort, err := c.resolveDestPort(cctx, funnelPort)
	if err != nil {
		return nil, err
	}

	log.Info().
		Str("container", cctx.containerName).
		Str("funnel_container_port", funnelPort).
		Str("funnel_host_port", funnelTargetPort).
		Str("funnel_public_port", funnelFunnelPort).
		Str("funnel_protocol", funnelProtocol).
		Str("funnel_path", funnelPath).
		Msg("Funnel enabled for public internet access")

	return &funnelConfig{
		IPAddress:  funnelDestIP,
		Port:       funnelPort,
		TargetPort: funnelTargetPort,
		PublicPort: funnelFunnelPort,
		Protocol:   funnelProtocol,
		Path:       funnelPath,
	}, nil
}

// parseContainer extracts service configuration from container labels.
// Returns one ContainerService for the primary port plus one for each indexed port.
// parseTags parses a comma-separated docktail.tags label value into a
// deduplicated tag list, falling back to a copy of defaultTags when the label
// is empty. Duplicates must be dropped: the Control Plane stores tags as a
// set, so a desired list with duplicates would register as permanent drift
// and trigger a re-PUT on every reconcile cycle.
func parseTags(tagsStr string, containerName string, defaultTags []string) []string {
	if tagsStr == "" {
		tags := make([]string, len(defaultTags))
		copy(tags, defaultTags)
		return tags
	}

	var tags []string
	seen := make(map[string]struct{})
	for _, part := range strings.Split(tagsStr, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		if !strings.HasPrefix(trimmed, "tag:") {
			log.Warn().
				Str("container", containerName).
				Str("tag", trimmed).
				Msg("Tag should start with 'tag:' prefix per Tailscale convention")
		}
		tags = append(tags, trimmed)
	}
	return tags
}

func (c *Client) parseContainer(ctx context.Context, containerID string, labels map[string]string) ([]*apptypes.ContainerService, error) {
	serviceEnabled := isServiceEnabled(labels)
	funnelEnabled := isFunnelEnabled(labels)
	if !serviceEnabled && !funnelEnabled {
		return nil, nil
	}

	// Get container details for port bindings
	inspect, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container: %w", err)
	}

	containerName := strings.TrimPrefix(inspect.Name, "/")

	cctx := &containerCtx{
		containerID:      containerID,
		containerName:    containerName,
		specifiedNetwork: labels[apptypes.LabelNetwork],
		inspect:          inspect,
		isHostNetwork:    inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "host",
		isNoNetwork:      inspect.HostConfig != nil && string(inspect.HostConfig.NetworkMode) == "none",
		isDirectMode:     labels[apptypes.LabelDirect] != "false",
	}

	cctx.tags = parseTags(labels[apptypes.LabelTags], cctx.containerName, c.defaultTags)

	var result []*apptypes.ContainerService
	if serviceEnabled {
		// Validate required labels
		serviceName := labels[apptypes.LabelService]
		if serviceName == "" {
			return nil, fmt.Errorf("missing required label: %s", apptypes.LabelService)
		}

		targetPort := labels[apptypes.LabelTarget]
		if targetPort == "" {
			return nil, fmt.Errorf("missing required label: %s", apptypes.LabelTarget)
		}

		// Resolve protocols for the primary port
		protocol, port, serviceProtocol, err := resolveProtocols(
			containerID, targetPort,
			labels[apptypes.LabelPort],
			labels[apptypes.LabelServiceProtocol],
			labels[apptypes.LabelTargetProtocol],
		)
		if err != nil {
			return nil, err
		}

		// Resolve destination for primary port
		destIP, destPort, err := c.resolveDestPort(cctx, targetPort)
		if err != nil {
			return nil, err
		}
		cctx.destIP = destIP
		monIP, monPort := c.monitorTarget(ctx, cctx, targetPort)

		proxyProtocol, err := parseProxyProtocol(labels[apptypes.LabelProxyProtocol], serviceProtocol)
		if err != nil {
			return nil, err
		}

		primary := &apptypes.ContainerService{
			ContainerID:        cctx.containerID[:12],
			ContainerName:      cctx.containerName,
			ServiceEnabled:     true,
			ServiceName:        serviceName,
			ServiceDescription: labels[apptypes.LabelDescription],
			Port:               port,
			TargetPort:         destPort,
			ServiceProtocol:    serviceProtocol,
			Protocol:           protocol,
			ProxyProtocol:      proxyProtocol,
			Tags:               cctx.tags,
			IPAddress:          destIP,
			MonitorIP:          monIP,
			MonitorPort:        monPort,
		}
		result = append(result, primary)

		// Parse indexed services (one container can define multiple separate Tailscale services)
		indexedServices, err := c.parseIndexedPorts(ctx, cctx, labels, serviceName, port)
		if err != nil {
			return nil, err
		}
		result = append(result, indexedServices...)
	}

	funnelCfg, err := c.parseFunnelConfig(cctx, labels)
	if err != nil {
		return nil, err
	}
	if funnelCfg == nil {
		return result, nil
	}

	if serviceEnabled {
		result[0].FunnelEnabled = true
		result[0].FunnelPort = funnelCfg.Port
		result[0].FunnelTargetPort = funnelCfg.TargetPort
		result[0].FunnelFunnelPort = funnelCfg.PublicPort
		result[0].FunnelProtocol = funnelCfg.Protocol
		result[0].FunnelPath = funnelCfg.Path
		return result, nil
	}

	result = append(result, &apptypes.ContainerService{
		ContainerID:      cctx.containerID[:12],
		ContainerName:    cctx.containerName,
		ServiceEnabled:   false,
		Tags:             cctx.tags,
		IPAddress:        funnelCfg.IPAddress,
		FunnelEnabled:    true,
		FunnelPort:       funnelCfg.Port,
		FunnelTargetPort: funnelCfg.TargetPort,
		FunnelFunnelPort: funnelCfg.PublicPort,
		FunnelProtocol:   funnelCfg.Protocol,
		FunnelPath:       funnelCfg.Path,
	})

	return result, nil
}

// parseIndexedPorts scans labels for indexed service definitions (docktail.service.N.*)
// and returns a ContainerService for each valid index. Each index defines a separate
// Tailscale service and requires its own name (docktail.service.N.name).
func (c *Client) parseIndexedPorts(
	ctx context.Context,
	cctx *containerCtx,
	labels map[string]string,
	primaryServiceName string,
	primaryServicePort string,
) ([]*apptypes.ContainerService, error) {
	// Collect all indices from labels
	indices := map[int]bool{}
	for key := range labels {
		if matches := indexedPortRegex.FindStringSubmatch(key); matches != nil {
			idx, err := strconv.Atoi(matches[1])
			if err != nil {
				continue
			}
			indices[idx] = true
		}
	}

	if len(indices) == 0 {
		return nil, nil
	}

	// Sort indices for deterministic processing
	sorted := make([]int, 0, len(indices))
	for idx := range indices {
		sorted = append(sorted, idx)
	}
	sort.Ints(sorted)

	log.Info().
		Str("container", cctx.containerName).
		Int("indexed_services", len(sorted)).
		Msg("Found indexed service definitions")

	// Track service name+port combos to detect duplicates.
	// Scoped by service name so different services can use the same port.
	usedServicePorts := map[string]int{}
	usedServicePorts[primaryServiceName+":"+primaryServicePort] = 0

	var services []*apptypes.ContainerService
	for _, idx := range sorted {
		prefix := fmt.Sprintf("docktail.service.%d.", idx)

		idxServiceName := labels[prefix+"name"]
		if idxServiceName == "" {
			log.Warn().
				Str("container", cctx.containerName).
				Int("index", idx).
				Msg("Missing required name label for indexed service, skipping")
			continue
		}

		targetPort := labels[prefix+"port"]
		if targetPort == "" {
			continue
		}

		idxServicePort := labels[prefix+"service-port"]
		idxServiceProtocol := labels[prefix+"service-protocol"]
		idxProtocol := labels[prefix+"protocol"]

		protocol, servicePort, serviceProtocol, err := resolveProtocols(
			cctx.containerID, targetPort, idxServicePort, idxServiceProtocol, idxProtocol,
		)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Msg("Failed to resolve protocols for indexed service, skipping")
			continue
		}

		idxProxyProtocol, err := parseProxyProtocol(labels[prefix+"proxy-protocol"], serviceProtocol)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Msg("Invalid proxy-protocol for indexed service, skipping")
			continue
		}

		// Check for duplicate service name + port combo
		dedupKey := idxServiceName + ":" + servicePort
		if prevIdx, exists := usedServicePorts[dedupKey]; exists {
			log.Warn().
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Int("conflicts_with", prevIdx).
				Str("service_port", servicePort).
				Msg("Duplicate service name and port across indices, skipping")
			continue
		}
		usedServicePorts[dedupKey] = idx

		// Resolve destination port
		idxDestIP, idxDestPort, err := c.resolveDestPort(cctx, targetPort)
		if err != nil {
			log.Warn().
				Err(err).
				Str("container", cctx.containerName).
				Str("service", idxServiceName).
				Int("index", idx).
				Str("target_port", targetPort).
				Msg("Failed to resolve destination for indexed service, skipping")
			continue
		}

		// In direct mode, reuse the primary port's container IP to avoid redundant
		// getContainerIP calls — all ports on the same container share one IP.
		if cctx.isDirectMode && cctx.destIP != "" {
			idxDestIP = cctx.destIP
		}

		monIP, monPort := c.monitorTarget(ctx, cctx, targetPort)

		svc := &apptypes.ContainerService{
			ContainerID:        cctx.containerID[:12],
			ContainerName:      cctx.containerName,
			ServiceEnabled:     true,
			ServiceName:        idxServiceName,
			ServiceDescription: labels[prefix+"description"],
			Port:               servicePort,
			TargetPort:         idxDestPort,
			ServiceProtocol:    serviceProtocol,
			Protocol:           protocol,
			ProxyProtocol:      idxProxyProtocol,
			Tags:               cctx.tags,
			IPAddress:          idxDestIP,
			MonitorIP:          monIP,
			MonitorPort:        monPort,
			FunnelEnabled:      false,
		}

		services = append(services, svc)

		log.Info().
			Str("container", cctx.containerName).
			Str("service", idxServiceName).
			Int("index", idx).
			Str("target_port", targetPort).
			Str("service_port", servicePort).
			Str("service_protocol", serviceProtocol).
			Str("protocol", protocol).
			Msg("Parsed indexed service")
	}

	return services, nil
}

// getContainerIP extracts the container's IP address from the specified or default network
func (c *Client) getContainerIP(inspect container.InspectResponse, specifiedNetwork string, containerName string) (string, string, error) {
	if inspect.NetworkSettings == nil || inspect.NetworkSettings.Networks == nil {
		return "", "", fmt.Errorf("container '%s' has no network settings", containerName)
	}

	networks := inspect.NetworkSettings.Networks

	// If a specific network is specified, use it
	if specifiedNetwork != "" {
		// Try exact match first
		if network, ok := networks[specifiedNetwork]; ok {
			if network.IPAddress == "" {
				return "", "", fmt.Errorf("container '%s' has no IP address on network '%s'", containerName, specifiedNetwork)
			}
			return network.IPAddress, specifiedNetwork, nil
		}

		// Try suffix match (handles docker-compose project prefixes like "projectname_backend")
		for networkName, network := range networks {
			if strings.HasSuffix(networkName, "_"+specifiedNetwork) {
				if network.IPAddress == "" {
					return "", "", fmt.Errorf("container '%s' has no IP address on network '%s'", containerName, networkName)
				}
				log.Debug().
					Str("container", containerName).
					Str("requested", specifiedNetwork).
					Str("matched", networkName).
					Msg("Matched network by suffix (docker-compose prefix detected)")
				return network.IPAddress, networkName, nil
			}
		}

		return "", "", fmt.Errorf("container '%s' is not connected to network '%s' (available: %v)", containerName, specifiedNetwork, getNetworkNames(networks))
	}

	// No network specified. Iterate in a stable (sorted) order rather than the
	// random Go map order, and prefer a network the agent's own container also
	// sits on so the resolved IP is actually reachable from the agent. A
	// multi-homed target otherwise gets an arbitrary IP that flips run to run;
	// dialing an address on a network the agent isn't attached to hits the
	// asymmetric-routing trap — the SYN leaves via the host gateway but the
	// target replies from its other interface, so the handshake never completes
	// and the local check times out at random. Priority: shared network >
	// bridge > first available.
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)

	c.ensureSelfNetIDs(context.Background())
	if len(c.selfNetIDs) > 0 {
		for _, name := range names {
			network := networks[name]
			if network.IPAddress == "" {
				continue
			}
			if _, ok := c.selfNetIDs[network.NetworkID]; ok {
				log.Debug().
					Str("container", containerName).
					Str("network", name).
					Str("ip", network.IPAddress).
					Msg("Using network shared with the agent for direct mode")
				return network.IPAddress, name, nil
			}
		}
	}

	// No shared network (or the agent isn't containerised): bridge > first available.
	if network, ok := networks["bridge"]; ok && network.IPAddress != "" {
		return network.IPAddress, "bridge", nil
	}

	for _, name := range names {
		network := networks[name]
		if network.IPAddress != "" {
			log.Debug().
				Str("container", containerName).
				Str("network", name).
				Str("ip", network.IPAddress).
				Msg("Using first available network for direct mode")
			return network.IPAddress, name, nil
		}
	}

	return "", "", fmt.Errorf("container '%s' has no IP address on any network", containerName)
}

// getNetworkNames returns a list of network names from the networks map
func getNetworkNames[V any](networks map[string]V) []string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	return names
}

// checkReachability performs a quick TCP connection test (best-effort, non-blocking)
func (c *Client) checkReachability(ip string, port string) error {
	address := net.JoinHostPort(ip, port)
	conn, err := net.DialTimeout("tcp", address, 1*time.Second)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
