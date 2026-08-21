package tailscale

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
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

func TestSameStringSet(t *testing.T) {
	tests := []struct {
		name string
		a    []string
		b    []string
		want bool
	}{
		{name: "equal ignoring order", a: []string{"tag:a", "tag:b"}, b: []string{"tag:b", "tag:a"}, want: true},
		{name: "duplicates do not count as difference", a: []string{"tag:a", "tag:a", "tag:b"}, b: []string{"tag:b", "tag:a"}, want: true},
		{name: "missing element", a: []string{"tag:a"}, b: []string{"tag:a", "tag:b"}, want: false},
		{name: "duplicates cannot mask a missing element", a: []string{"tag:a", "tag:b"}, b: []string{"tag:b", "tag:b"}, want: false},
		{name: "extra element", a: []string{"tag:a", "tag:b"}, b: []string{"tag:a"}, want: false},
		{name: "both empty", a: nil, b: []string{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameStringSet(tt.a, tt.b); got != tt.want {
				t.Errorf("sameStringSet(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := sameStringSet(tt.b, tt.a); got != tt.want {
				t.Errorf("sameStringSet(%v, %v) = %v, want %v", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

// servicePutPayload mirrors the JSON body SyncServiceDefinition PUTs to the
// Control Plane API.
type servicePutPayload struct {
	Name    string   `json:"name"`
	Addrs   []string `json:"addrs"`
	Tags    []string `json:"tags"`
	Ports   []string `json:"ports"`
	Comment string   `json:"comment"`
}

func TestSyncServiceDefinitionReconciliation(t *testing.T) {
	tests := []struct {
		name        string
		existing    *apiService // nil = service does not exist (GET returns 404)
		tags        []string
		ports       []string
		description string
		wantPut     bool
		want        servicePutPayload
	}{
		{
			name: "tag drift is reconciled, ports addrs and manual comment preserved",
			existing: &apiService{
				Name:    "svc:web",
				Addrs:   []string{"100.100.1.1"},
				Tags:    []string{"tag:container"},
				Ports:   []string{"tcp:443"},
				Comment: "manual note",
			},
			tags:    []string{"tag:web", "tag:production"},
			ports:   []string{"443", "8080"},
			wantPut: true,
			want: servicePutPayload{
				Name:    "svc:web",
				Addrs:   []string{"100.100.1.1"},
				Tags:    []string{"tag:web", "tag:production"},
				Ports:   []string{"tcp:443"},
				Comment: "manual note",
			},
		},
		{
			name: "identical tags in different order are not drift",
			existing: &apiService{
				Name: "svc:web",
				Tags: []string{"tag:b", "tag:a"},
			},
			tags:    []string{"tag:a", "tag:b"},
			wantPut: false,
		},
		{
			name: "empty desired tags leave existing tags alone",
			existing: &apiService{
				Name: "svc:web",
				Tags: []string{"tag:manual"},
			},
			tags:    nil,
			wantPut: false,
		},
		{
			name: "duplicate desired tags are not drift against the deduped stored set",
			existing: &apiService{
				Name: "svc:web",
				Tags: []string{"tag:web"},
			},
			tags:    []string{"tag:web", "tag:web"},
			wantPut: false,
		},
		{
			name: "tag and description drift update together",
			existing: &apiService{
				Name:    "svc:web",
				Tags:    []string{"tag:container"},
				Ports:   []string{"tcp:80"},
				Comment: "old",
			},
			tags:        []string{"tag:web"},
			description: "new",
			wantPut:     true,
			want: servicePutPayload{
				Name:    "svc:web",
				Tags:    []string{"tag:web"},
				Ports:   []string{"tcp:80"},
				Comment: "new",
			},
		},
		{
			name: "description drift alone keeps existing tags",
			existing: &apiService{
				Name:  "svc:web",
				Tags:  []string{"tag:manual", "tag:extra"},
				Ports: []string{"tcp:80"},
			},
			tags:        []string{"tag:manual", "tag:extra"},
			description: "described",
			wantPut:     true,
			want: servicePutPayload{
				Name:    "svc:web",
				Tags:    []string{"tag:manual", "tag:extra"},
				Ports:   []string{"tcp:80"},
				Comment: "described",
			},
		},
		{
			name: "fully converged service is not rewritten",
			existing: &apiService{
				Name:    "svc:web",
				Tags:    []string{"tag:web"},
				Comment: "same",
			},
			tags:        []string{"tag:web"},
			description: "same",
			wantPut:     false,
		},
		{
			name:        "missing service is created with tcp-prefixed ports",
			existing:    nil,
			tags:        []string{"tag:web"},
			ports:       []string{"443", "8080"},
			description: "hello",
			wantPut:     true,
			want: servicePutPayload{
				Name:    "svc:web",
				Tags:    []string{"tag:web"},
				Ports:   []string{"tcp:443", "tcp:8080"},
				Comment: "hello",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPut *servicePutPayload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					if tt.existing == nil {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(tt.existing)
				case http.MethodPut:
					var payload servicePutPayload
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Errorf("failed to decode PUT payload: %v", err)
					}
					gotPut = &payload
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected %s request", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()

			c := &Client{
				baseURL:    srv.URL,
				tailnet:    "example.com",
				httpClient: srv.Client(),
			}

			if err := c.SyncServiceDefinition(context.Background(), "web", tt.tags, tt.ports, tt.description); err != nil {
				t.Fatalf("SyncServiceDefinition returned error: %v", err)
			}

			if !tt.wantPut {
				if gotPut != nil {
					t.Fatalf("unexpected PUT with payload %+v", *gotPut)
				}
				return
			}
			if gotPut == nil {
				t.Fatal("expected a PUT to the Control Plane, got none")
			}
			if gotPut.Name != tt.want.Name {
				t.Errorf("name = %q, want %q", gotPut.Name, tt.want.Name)
			}
			if !slices.Equal(gotPut.Addrs, tt.want.Addrs) {
				t.Errorf("addrs = %v, want %v", gotPut.Addrs, tt.want.Addrs)
			}
			if !slices.Equal(gotPut.Tags, tt.want.Tags) {
				t.Errorf("tags = %v, want %v", gotPut.Tags, tt.want.Tags)
			}
			if !slices.Equal(gotPut.Ports, tt.want.Ports) {
				t.Errorf("ports = %v, want %v", gotPut.Ports, tt.want.Ports)
			}
			if gotPut.Comment != tt.want.Comment {
				t.Errorf("comment = %q, want %q", gotPut.Comment, tt.want.Comment)
			}
		})
	}
}

func TestDeleteUnusedServiceDefinitionsUsesAdvertisingHosts(t *testing.T) {
	const (
		orphanService     = "svc:orphan"
		advertisedService = "svc:advertised-elsewhere"
	)

	hostChecks := make(map[string]int)
	var deleted []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tailnet/example.com/services":
			_ = json.NewEncoder(w).Encode(struct {
				VIPServices []apiService `json:"vipServices"`
			}{
				VIPServices: []apiService{
					{Name: orphanService},
					{Name: advertisedService},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tailnet/example.com/services/"+orphanService+"/devices":
			hostChecks[orphanService]++
			_ = json.NewEncoder(w).Encode(struct {
				Hosts []serviceHost `json:"hosts"`
			}{Hosts: []serviceHost{}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/tailnet/example.com/services/"+advertisedService+"/devices":
			hostChecks[advertisedService]++
			// Keep this as literal API-shaped JSON. Encoding serviceHost here
			// would let a wrong struct tag change both production and the fixture
			// together, masking the contract regression.
			_, _ = w.Write([]byte(`{"hosts":[{"nodeId":"node-foreign"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2/tailnet/example.com/services/"+orphanService:
			deleted = append(deleted, orphanService)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s request to %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{
		baseURL:         srv.URL,
		tailnet:         "example.com",
		httpClient:      srv.Client(),
		ignoredServices: make(map[string]struct{}),
	}

	if err := c.deleteUnusedServiceDefinitions(context.Background(), nil); err != nil {
		t.Fatalf("deleteUnusedServiceDefinitions returned error: %v", err)
	}

	if !slices.Equal(deleted, []string{orphanService}) {
		t.Fatalf("deleted services = %v, want only %q", deleted, orphanService)
	}
	if hostChecks[orphanService] != 1 {
		t.Errorf("orphan host checks = %d, want 1", hostChecks[orphanService])
	}
	if hostChecks[advertisedService] != 1 {
		t.Errorf("advertised service host checks = %d, want 1", hostChecks[advertisedService])
	}
}
