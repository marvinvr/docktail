package docker

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/container"
	network "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// Swarm discovery mode.
//
// GetEnabledContainers only sees containers running on the agent's own Docker
// node — the /containers endpoint is node-local in a Swarm cluster, on manager
// nodes as much as workers. An agent can therefore only advertise services whose
// containers are colocated with it, which forces one agent per node for a
// cluster-wide setup.
//
// The Swarm API does offer a cluster view, but at service granularity:
// /services returns every service in the cluster together with its labels, and
// each service carries Endpoint.VirtualIPs — the VIP the cluster's routing mesh
// already keeps alive and load-balanced across that service's tasks, on the
// overlay network the tasks are attached to. A VIP is reachable from any node
// attached to that overlay, so one agent can advertise a whole cluster as long
// as the overlay carries the traffic.
//
// Labels come from Spec.TaskTemplate.ContainerSpec.Labels, which is where both
// `labels:` and `deploy.labels:` from a compose file land, so a service is
// labelled exactly like the equivalent standalone container would be.

// swarmNetworkEnv names the Docker network whose VIP should be advertised when
// a service is attached to several. Optional: without it, the lowest VIP
// (deterministically, by address) wins.
const swarmNetworkEnv = "DOCKTAIL_SWARM_NETWORK"

// GetEnabledSwarmServices returns the Swarm services managed by DockTail,
// discovered across the whole cluster rather than only on this node.
//
// It requires a manager endpoint: /services is manager-only. On a worker-only
// agent it fails, and the caller should treat that as "cannot run in swarm
// mode" rather than retrying.
func (c *Client) GetEnabledSwarmServices(ctx context.Context) ([]*apptypes.ContainerService, error) {
	services, err := c.cli.ServiceList(ctx, swarm.ServiceListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list swarm services (is this Docker endpoint a swarm manager?): %w", err)
	}

	var managed []*apptypes.ContainerService
	for _, svc := range services {
		labels := swarmServiceLabels(svc)
		if !isManagedContainer(labels) {
			continue
		}

		parsed, err := c.parseSwarmService(ctx, svc, labels)
		if err != nil {
			log.Warn().
				Err(err).
				Str("service", svc.Spec.Name).
				Msg("Failed to parse swarm service, skipping")
			continue
		}
		managed = append(managed, parsed...)
	}

	return managed, nil
}

// swarmServiceLabels returns the labels a Swarm service should be configured
// from: the task template's container labels, which is where compose `labels:`
// and `deploy.labels:` both land. Returns nil for a service with no task
// template (an externally-managed service).
func swarmServiceLabels(svc swarm.Service) map[string]string {
	if svc.Spec.TaskTemplate.ContainerSpec == nil {
		return nil
	}
	return svc.Spec.TaskTemplate.ContainerSpec.Labels
}

// parseSwarmService turns one Swarm service into its Tailscale services. The
// service VIP stands in for the container IP: it is the cluster-stable address
// for those tasks and the one an agent on any node can dial.
func (c *Client) parseSwarmService(ctx context.Context, svc swarm.Service, labels map[string]string) ([]*apptypes.ContainerService, error) {
	networkID, vip, err := c.selectServiceVIP(ctx, svc)
	if err != nil {
		return nil, err
	}

	// A Swarm service is network_mode: none only when it has no network at all,
	// in which case there is no VIP to advertise and selectServiceVIP has
	// already failed. host-mode tasks are reachable through the service's
	// published ports rather than a VIP; that is the one case where direct mode
	// cannot work, so it is rejected here with the same guidance the container
	// path gives.
	inspect := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:   svc.ID,
			Name: "/" + svc.Spec.Name,
			HostConfig: &container.HostConfig{
				NetworkMode: container.NetworkMode(swarmNetworkMode(svc)),
			},
		},
		Config: &container.Config{Labels: labels},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				networkNameResolver(c, networkID): {
					NetworkID: networkID,
					IPAddress: vip,
				},
			},
		},
	}

	services, err := c.parseFromInspect(ctx, svc.ID, inspect, labels)
	if err != nil {
		return nil, err
	}
	return withCloudLabels(services, labels), nil
}

// selectServiceVIP picks the overlay VIP to advertise for a service. A service
// attached to several networks gets one VIP per network; only the one on a
// network the agent is also attached to is dialable, so the agent's own
// networks win, then an operator-pinned network, then a deterministic choice.
func (c *Client) selectServiceVIP(ctx context.Context, svc swarm.Service) (networkID, vip string, err error) {
	// Resolve the networks the agent is attached to so the chosen VIP is one
	// this agent can actually dial, then defer to the pure selection logic.
	c.ensureSelfNetIDs(ctx)
	return c.selectServiceVIPFrom(svc, c.selfNetIDs)
}

// selectServiceVIPFrom is selectServiceVIP with the set of network IDs the
// agent is attached to passed in, so the choice logic can be exercised without
// a live Docker daemon.
func (c *Client) selectServiceVIPFrom(svc swarm.Service, sharedNetIDs map[string]struct{}) (networkID, vip string, err error) {
	if len(svc.Endpoint.VirtualIPs) == 0 {
		return "", "", fmt.Errorf("service %s has no virtual IP: a service only gets one in vip mode "+
			"(replicated or global). Publish the port or use a network the service is attached to", svc.Spec.Name)
	}

	candidates := make([]swarm.EndpointVirtualIP, 0, len(svc.Endpoint.VirtualIPs))
	candidates = append(candidates, svc.Endpoint.VirtualIPs...)
	sort.Slice(candidates, func(i, j int) bool {
		return lessIP(candidates[i].Addr, candidates[j].Addr)
	})

	if preferred := strings.TrimSpace(getSwarmNetworkEnv()); preferred != "" {
		for _, v := range candidates {
			if networkNameResolver(c, v.NetworkID) == preferred {
				return v.NetworkID, stripCIDR(v.Addr), nil
			}
		}
		return "", "", fmt.Errorf("service %s is not attached to network %q (attached to: %v)",
			svc.Spec.Name, preferred, c.swarmNetworkNames(svc))
	}

	if len(sharedNetIDs) > 0 {
		for _, v := range candidates {
			if _, ok := sharedNetIDs[v.NetworkID]; ok {
				return v.NetworkID, stripCIDR(v.Addr), nil
			}
		}
	}

	return candidates[0].NetworkID, stripCIDR(candidates[0].Addr), nil
}

// swarmNetworkEnvVar is a package-level indirection so tests can pin the
// operator's network preference without touching the process environment.
var swarmNetworkEnvVar = func() string { return os.Getenv(swarmNetworkEnv) }

func getSwarmNetworkEnv() string { return swarmNetworkEnvVar() }

// swarmNetworkNames resolves the Docker network names a service is attached to.
// The alias in a service's NetworkAttachmentConfig is the DNS name the *service*
// gets on that network, not the network's own name, so it cannot answer
// DOCKTAIL_SWARM_NETWORK. Names come from inspecting each attached network.
// A network that cannot be inspected (one the agent has no permission for, or
// that has since been removed) falls back to its ID so the error message names
// something the operator can look up.
func (c *Client) swarmNetworkNames(svc swarm.Service) []string {
	names := make([]string, 0, len(svc.Spec.TaskTemplate.Networks))
	for _, n := range svc.Spec.TaskTemplate.Networks {
		names = append(names, networkNameResolver(c, n.Target))
	}
	sort.Strings(names)
	return names
}

// networkNameResolver maps a Docker network ID to its network name. Overridden
// in tests so the selection logic can be exercised without a live daemon.
var networkNameResolver = func(c *Client, networkID string) string { return c.networkName(networkID) }

// networkName resolves one network ID to its Docker network name, caching the
// answer for the life of the agent. Names rarely change, and the lookup costs
// an API call per candidate per reconcile otherwise.
func (c *Client) networkName(networkID string) string {
	if networkID == "host" {
		return "host"
	}
	if name, ok := c.netNames.Load(networkID); ok {
		return name.(string) //nolint:forcetypeassert // only strings are ever stored
	}

	name := networkID
	if inspect, _, err := c.cli.NetworkInspectWithRaw(context.Background(), networkID, network.InspectOptions{}); err == nil && inspect.Name != "" {
		name = inspect.Name
	} else {
		log.Debug().
			Err(err).
			Str("network_id", networkID).
			Msg("Could not resolve network name; using its ID")
	}

	c.netNames.Store(networkID, name)
	return name
}

// swarmNetworkMode reports the effective network mode of a Swarm service's
// tasks: "host" when every attached network is the host network, "" otherwise.
func swarmNetworkMode(svc swarm.Service) string {
	networks := svc.Spec.TaskTemplate.Networks
	if len(networks) == 0 {
		return ""
	}
	for _, n := range networks {
		if n.Target != "host" {
			return ""
		}
	}
	return "host"
}

// stripCIDR turns "10.0.4.7/24" into "10.0.4.7". Swarm reports VirtualIPs in
// CIDR form; everything downstream (tailscale serve, the health check) wants a
// bare address.
func stripCIDR(addr string) string {
	if ip, _, err := net.ParseCIDR(addr); err == nil {
		return ip.String()
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// lessIP orders two CIDR addresses numerically when possible, falling back to
// string order so the choice stays deterministic either way.
func lessIP(a, b string) bool {
	ipa := net.ParseIP(stripCIDR(a))
	ipb := net.ParseIP(stripCIDR(b))
	if ipa == nil || ipb == nil {
		return a < b
	}
	return bytes.Compare(ipa, ipb) < 0
}
