package tailscale

import (
	"testing"

	apptypes "github.com/marvinvr/docktail/types"
)

func TestAggregateServiceDefinitions(t *testing.T) {
	tests := []struct {
		name     string
		services []*apptypes.ContainerService
		expected map[string]*serviceDef
	}{
		{
			name: "carries description onto the service definition",
			services: []*apptypes.ContainerService{
				{ServiceEnabled: true, ServiceName: "linkding", Port: "443", Tags: []string{"tag:container"}, ServiceDescription: "Bookmark Manager"},
			},
			expected: map[string]*serviceDef{
				"linkding": {Tags: []string{"tag:container"}, Ports: []string{"443"}, Description: "Bookmark Manager"},
			},
		},
		{
			name: "empty description stays empty",
			services: []*apptypes.ContainerService{
				{ServiceEnabled: true, ServiceName: "web", Port: "80", Tags: []string{"tag:container"}},
			},
			expected: map[string]*serviceDef{
				"web": {Tags: []string{"tag:container"}, Ports: []string{"80"}, Description: ""},
			},
		},
		{
			name: "aggregates ports and keeps first non-empty description per service",
			services: []*apptypes.ContainerService{
				{ServiceEnabled: true, ServiceName: "web", Port: "443", ServiceDescription: ""},
				{ServiceEnabled: true, ServiceName: "web", Port: "8080", ServiceDescription: "Primary Web UI"},
				{ServiceEnabled: true, ServiceName: "web", Port: "443", ServiceDescription: "ignored duplicate"},
			},
			expected: map[string]*serviceDef{
				"web": {Ports: []string{"443", "8080"}, Description: "Primary Web UI"},
			},
		},
		{
			name: "indexed services each keep their own description",
			services: []*apptypes.ContainerService{
				{ServiceEnabled: true, ServiceName: "qbittorrent", Port: "8000", ServiceDescription: "Torrent client"},
				{ServiceEnabled: true, ServiceName: "bitmagnet", Port: "8001", ServiceDescription: "DHT crawler"},
			},
			expected: map[string]*serviceDef{
				"qbittorrent": {Ports: []string{"8000"}, Description: "Torrent client"},
				"bitmagnet":   {Ports: []string{"8001"}, Description: "DHT crawler"},
			},
		},
		{
			name: "disabled services are ignored",
			services: []*apptypes.ContainerService{
				{ServiceEnabled: false, ServiceName: "funnel-only", Port: "443", ServiceDescription: "should be skipped"},
			},
			expected: map[string]*serviceDef{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := aggregateServiceDefinitions(tt.services)

			if len(got) != len(tt.expected) {
				t.Fatalf("got %d services, want %d", len(got), len(tt.expected))
			}

			for name, wantDef := range tt.expected {
				gotDef, ok := got[name]
				if !ok {
					t.Fatalf("missing service %q", name)
				}
				if gotDef.Description != wantDef.Description {
					t.Errorf("service %q description = %q, want %q", name, gotDef.Description, wantDef.Description)
				}
				if len(gotDef.Ports) != len(wantDef.Ports) {
					t.Errorf("service %q ports = %v, want %v", name, gotDef.Ports, wantDef.Ports)
				} else {
					for i, p := range wantDef.Ports {
						if gotDef.Ports[i] != p {
							t.Errorf("service %q ports[%d] = %q, want %q", name, i, gotDef.Ports[i], p)
						}
					}
				}
			}
		})
	}
}
