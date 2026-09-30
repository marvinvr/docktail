package docker

import (
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

// stubNetworkNames makes network-ID -> name resolution deterministic without a
// live Docker daemon. IDs in the tests are netA/netB, so name them by a lookup
// table the test supplies.
func stubNetworkNames(t *testing.T, names map[string]string) {
	t.Helper()
	prev := networkNameResolver
	networkNameResolver = func(_ *Client, networkID string) string {
		if name, ok := names[networkID]; ok {
			return name
		}
		return networkID
	}
	t.Cleanup(func() { networkNameResolver = prev })
}

func newTestService(networks []swarm.NetworkAttachmentConfig, vips []swarm.EndpointVirtualIP) swarm.Service {
	return swarm.Service{
		Spec: swarm.ServiceSpec{
			Annotations:  swarm.Annotations{Name: "entertainment_plex"},
			TaskTemplate: swarm.TaskSpec{Networks: networks},
		},
		Endpoint: swarm.Endpoint{VirtualIPs: vips},
	}
}

func TestStripCIDR(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "strips the prefix length", in: "10.0.4.7/24", want: "10.0.4.7"},
		{name: "passes a bare address through", in: "10.0.4.7", want: "10.0.4.7"},
		{name: "keeps a /128 host address", in: "fd7a::1/128", want: "fd7a::1"},
		{name: "drops a port", in: "10.0.4.7:8080", want: "10.0.4.7"},
		{name: "returns garbage unchanged", in: "not-an-ip", want: "not-an-ip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripCIDR(tt.in); got != tt.want {
				t.Errorf("stripCIDR(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLessIP(t *testing.T) {
	// Numeric, not lexicographic: 10.0.4.9 must sort before 10.0.4.10.
	if !lessIP("10.0.4.9/24", "10.0.4.10/24") {
		t.Error("10.0.4.9 should sort before 10.0.4.10 (numeric, not string order)")
	}
	if lessIP("10.0.4.10/24", "10.0.4.9/24") {
		t.Error("10.0.4.10 should not sort before 10.0.4.9")
	}
	if !lessIP("10.0.4.2/24", "10.0.10.2/24") {
		t.Error("10.0.4.2 should sort before 10.0.10.2")
	}
	// Unparseable addresses fall back to string order, which must still be a
	// strict ordering so sort.Slice does not misbehave.
	if lessIP("aaa", "bbb") == lessIP("bbb", "aaa") {
		t.Error("string fallback should order aaa before bbb")
	}
}

func TestSwarmServiceLabels(t *testing.T) {
	tests := []struct {
		name string
		svc  swarm.Service
		want map[string]string
	}{
		{
			name: "reads the task template container labels",
			svc: swarm.Service{
				Spec: swarm.ServiceSpec{
					TaskTemplate: swarm.TaskSpec{
						ContainerSpec: &swarm.ContainerSpec{
							Labels: map[string]string{
								"docktail.service.enable": "true",
								"docktail.service.name":   "plex",
							},
						},
					},
				},
			},
			want: map[string]string{
				"docktail.service.enable": "true",
				"docktail.service.name":   "plex",
			},
		},
		{
			name: "returns nil when there is no container spec",
			svc:  swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{}}},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := swarmServiceLabels(tt.svc)
			if len(got) != len(tt.want) {
				t.Fatalf("swarmServiceLabels() = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("label %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestSwarmNetworkNames(t *testing.T) {
	stubNetworkNames(t, map[string]string{"netA": "tailscale"})

	svc := swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
		Networks: []swarm.NetworkAttachmentConfig{
			{Target: "netA", Aliases: []string{"entertainment_plex"}},
			{Target: "netZ", Aliases: []string{"entertainment_plex"}},
		},
	}}}

	got := (&Client{}).swarmNetworkNames(svc)
	// Sorted, so the unresolved ID ("netZ") precedes the resolved name.
	want := []string{"netZ", "tailscale"}

	if len(got) != len(want) {
		t.Fatalf("swarmNetworkNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("name[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSwarmNetworkMode(t *testing.T) {
	tests := []struct {
		name string
		svc  swarm.Service
		want string
	}{
		{
			name: "overlay attachment is not host mode",
			svc: swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
				Networks: []swarm.NetworkAttachmentConfig{{Target: "netid", Aliases: []string{"tailscale"}}},
			}}},
			want: "",
		},
		{
			name: "every attachment on host means host mode",
			svc: swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{
				Networks: []swarm.NetworkAttachmentConfig{{Target: "host"}},
			}}},
			want: "host",
		},
		{
			name: "no attachment is not host mode",
			svc:  swarm.Service{Spec: swarm.ServiceSpec{TaskTemplate: swarm.TaskSpec{}}},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := swarmNetworkMode(tt.svc); got != tt.want {
				t.Errorf("swarmNetworkMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSelectServiceVIP(t *testing.T) {
	c := &Client{}

	t.Run("no virtual IP is an error, not a silent skip", func(t *testing.T) {
		_, _, err := c.selectServiceVIPFrom(newTestService(nil, nil), nil)
		if err == nil {
			t.Fatal("expected an error for a service without a VIP")
		}
	})

	t.Run("picks the lowest VIP when the agent shares no network", func(t *testing.T) {
		stubNetworkNames(t, map[string]string{"netA": "appnet"})
		svc := newTestService(
			[]swarm.NetworkAttachmentConfig{{Target: "netA", Aliases: []string{"plex"}}},
			[]swarm.EndpointVirtualIP{{NetworkID: "netA", Addr: "10.0.9.20/24"}},
		)
		id, vip, err := c.selectServiceVIPFrom(svc, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "netA" || vip != "10.0.9.20" {
			t.Errorf("got (%q, %q), want (\"netA\", \"10.0.9.20\")", id, vip)
		}
	})

	t.Run("prefers a network the agent is attached to", func(t *testing.T) {
		stubNetworkNames(t, map[string]string{"netA": "appnet", "netB": "tailscale"})
		svc := newTestService(
			[]swarm.NetworkAttachmentConfig{
				{Target: "netA", Aliases: []string{"plex"}},
				{Target: "netB", Aliases: []string{"plex"}},
			},
			[]swarm.EndpointVirtualIP{
				{NetworkID: "netA", Addr: "10.0.4.2/24"},
				{NetworkID: "netB", Addr: "10.0.10.9/24"},
			},
		)
		id, vip, err := c.selectServiceVIPFrom(svc, map[string]struct{}{"netB": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "netB" || vip != "10.0.10.9" {
			t.Errorf("got (%q, %q), want (\"netB\", \"10.0.10.9\") — the shared network must win", id, vip)
		}
	})

	t.Run("operator-pinned network overrides the shared-network preference", func(t *testing.T) {
		stubNetworkNames(t, map[string]string{"netA": "appnet", "netB": "tailscale"})
		t.Setenv(swarmNetworkEnv, "appnet")
		svc := newTestService(
			[]swarm.NetworkAttachmentConfig{
				{Target: "netA", Aliases: []string{"plex"}},
				{Target: "netB", Aliases: []string{"plex"}},
			},
			[]swarm.EndpointVirtualIP{
				{NetworkID: "netA", Addr: "10.0.4.2/24"},
				{NetworkID: "netB", Addr: "10.0.10.9/24"},
			},
		)
		id, vip, err := c.selectServiceVIPFrom(svc, map[string]struct{}{"netB": {}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "netA" || vip != "10.0.4.2" {
			t.Errorf("got (%q, %q), want (\"netA\", \"10.0.4.2\")", id, vip)
		}
	})

	t.Run("pinned network the service is not on is an error", func(t *testing.T) {
		stubNetworkNames(t, map[string]string{"netA": "appnet"})
		t.Setenv(swarmNetworkEnv, "absent")
		svc := newTestService(
			[]swarm.NetworkAttachmentConfig{{Target: "netA", Aliases: []string{"plex"}}},
			[]swarm.EndpointVirtualIP{{NetworkID: "netA", Addr: "10.0.4.2/24"}},
		)
		if _, _, err := c.selectServiceVIPFrom(svc, nil); err == nil {
			t.Fatal("expected an error when the pinned network is not attached")
		}
	})

	t.Run("host network has no VIP and is reported as such", func(t *testing.T) {
		stubNetworkNames(t, nil)
		svc := newTestService(
			[]swarm.NetworkAttachmentConfig{{Target: "host"}},
			nil,
		)
		if _, _, err := c.selectServiceVIPFrom(svc, nil); err == nil {
			t.Fatal("expected an error for a host-networked service with no VIP")
		}
	})
}
