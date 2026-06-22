package tailscale

import (
	"encoding/json"
	"testing"

	apptypes "github.com/marvinvr/docktail/types"
)

func TestTailscaleStatusParsing(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		expectedServices int
		checkFunc        func(t *testing.T, status TailscaleStatus)
	}{
		{
			name:             "empty services",
			input:            `{"Services":{}}`,
			expectedServices: 0,
		},
		{
			name: "single HTTPS service",
			input: `{
				"Services": {
					"svc:web": {
						"TCP": {
							"443": {"HTTPS": true}
						},
						"Web": {
							"https://svc:web:443": {
								"Handlers": {
									"/": {"Proxy": "http://172.17.0.2:8080"}
								}
							}
						}
					}
				}
			}`,
			expectedServices: 1,
			checkFunc: func(t *testing.T, status TailscaleStatus) {
				svc, ok := status.Services["svc:web"]
				if !ok {
					t.Fatal("expected svc:web to exist")
				}
				tcpCfg, ok := svc.TCP["443"]
				if !ok {
					t.Fatal("expected TCP port 443")
				}
				if !tcpCfg.HTTPS {
					t.Error("expected HTTPS=true")
				}
				if tcpCfg.HTTP {
					t.Error("expected HTTP=false")
				}
				webCfg, ok := svc.Web["https://svc:web:443"]
				if !ok {
					t.Fatal("expected web config for https://svc:web:443")
				}
				handler, ok := webCfg.Handlers["/"]
				if !ok {
					t.Fatal("expected handler for /")
				}
				if handler.Proxy != "http://172.17.0.2:8080" {
					t.Errorf("expected proxy http://172.17.0.2:8080, got %s", handler.Proxy)
				}
			},
		},
		{
			name: "single HTTP service",
			input: `{
				"Services": {
					"svc:api": {
						"TCP": {
							"80": {"HTTP": true}
						},
						"Web": {
							"http://svc:api:80": {
								"Handlers": {
									"/": {"Proxy": "http://172.17.0.3:3000"}
								}
							}
						}
					}
				}
			}`,
			expectedServices: 1,
			checkFunc: func(t *testing.T, status TailscaleStatus) {
				svc := status.Services["svc:api"]
				tcpCfg := svc.TCP["80"]
				if !tcpCfg.HTTP {
					t.Error("expected HTTP=true")
				}
				if tcpCfg.HTTPS {
					t.Error("expected HTTPS=false")
				}
			},
		},
		{
			name: "TCP service (no HTTP/HTTPS flags)",
			input: `{
				"Services": {
					"svc:db": {
						"TCP": {
							"5432": {"TCPForward": "172.17.0.5:5432"}
						},
						"Web": {}
					}
				}
			}`,
			expectedServices: 1,
			checkFunc: func(t *testing.T, status TailscaleStatus) {
				svc := status.Services["svc:db"]
				tcpCfg := svc.TCP["5432"]
				if tcpCfg.HTTP || tcpCfg.HTTPS {
					t.Error("expected both HTTP and HTTPS to be false for TCP service")
				}
				if tcpCfg.TCPForward != "172.17.0.5:5432" {
					t.Errorf("expected TCPForward 172.17.0.5:5432, got %q", tcpCfg.TCPForward)
				}
			},
		},
		{
			name: "multiple services",
			input: `{
				"Services": {
					"svc:web": {
						"TCP": {"443": {"HTTPS": true}},
						"Web": {}
					},
					"svc:api": {
						"TCP": {"80": {"HTTP": true}},
						"Web": {}
					},
					"manual-service": {
						"TCP": {"8080": {"HTTP": true}},
						"Web": {}
					}
				}
			}`,
			expectedServices: 3,
		},
		{
			name:             "null services field",
			input:            `{}`,
			expectedServices: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var status TailscaleStatus
			if err := json.Unmarshal([]byte(tt.input), &status); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if len(status.Services) != tt.expectedServices {
				t.Errorf("expected %d services, got %d", tt.expectedServices, len(status.Services))
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, status)
			}
		})
	}
}

func TestParseManagedServicesDestinations(t *testing.T) {
	status := TailscaleStatus{
		Services: map[string]TailscaleService{
			"svc:web": {
				TCP: map[string]TailscaleTCPConfig{
					"443": {HTTPS: true},
				},
				Web: map[string]TailscaleWebConfig{
					"https://svc:web:443": {
						Handlers: map[string]TailscaleHandler{
							"/": {Proxy: "http://172.17.0.2:8080"},
						},
					},
				},
			},
			"svc:db": {
				// Plain TCP service: destination lives on the TCP handler as
				// TCPForward, and the Web section is empty (issue #56).
				TCP: map[string]TailscaleTCPConfig{
					"5432": {TCPForward: "172.17.0.3:5432"},
				},
				Web: map[string]TailscaleWebConfig{},
			},
			"manual-service": {
				// Not managed by DockTail (no svc: prefix) -> ignored.
				TCP: map[string]TailscaleTCPConfig{
					"8080": {HTTP: true},
				},
			},
		},
	}

	got := parseManagedServices(status)

	if _, ok := got["manual-service:8080"]; ok {
		t.Error("expected unmanaged service to be excluded")
	}

	web, ok := got["svc:web:443"]
	if !ok {
		t.Fatal("expected svc:web:443 endpoint")
	}
	if web.Protocol != "https" {
		t.Errorf("svc:web protocol = %q, want https", web.Protocol)
	}
	if web.Destination != "http://172.17.0.2:8080" {
		t.Errorf("svc:web destination = %q, want http://172.17.0.2:8080", web.Destination)
	}

	db, ok := got["svc:db:5432"]
	if !ok {
		t.Fatal("expected svc:db:5432 endpoint")
	}
	if db.Protocol != "tcp" {
		t.Errorf("svc:db protocol = %q, want tcp", db.Protocol)
	}
	// The regression: TCP destination must be parsed from TCPForward, not left empty.
	if db.Destination != "172.17.0.3:5432" {
		t.Errorf("svc:db destination = %q, want 172.17.0.3:5432 (issue #56)", db.Destination)
	}
}

func TestSameDestination(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		expected string
		want     bool
	}{
		{
			name:     "tcp forward without scheme matches expected tcp url",
			current:  "172.17.0.3:5432",
			expected: "tcp://172.17.0.3:5432",
			want:     true,
		},
		{
			name:     "http destinations with matching scheme",
			current:  "http://172.17.0.2:8080",
			expected: "http://172.17.0.2:8080",
			want:     true,
		},
		{
			name:     "backend scheme change is detected",
			current:  "http://172.17.0.2:8080",
			expected: "https://172.17.0.2:8080",
			want:     false,
		},
		{
			name:     "different host:port never matches",
			current:  "172.17.0.3:5432",
			expected: "tcp://172.17.0.9:5432",
			want:     false,
		},
		{
			name:     "empty current forces reapply",
			current:  "",
			expected: "tcp://172.17.0.3:5432",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameDestination(tt.current, tt.expected); got != tt.want {
				t.Errorf("sameDestination(%q, %q) = %v, want %v", tt.current, tt.expected, got, tt.want)
			}
		})
	}
}

func TestFunnelStatusParsing(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedPorts int
		checkFunc     func(t *testing.T, status FunnelStatus)
	}{
		{
			name: "single HTTPS funnel",
			input: `{
				"TCP": {
					"443": {"HTTPS": true}
				},
				"Web": {
					"https://myhost.tail1234.ts.net:443": {
						"Handlers": {
							"/": {"Proxy": "http://127.0.0.1:8080"}
						}
					}
				},
				"AllowFunnel": {
					"myhost.tail1234.ts.net:443": true
				}
			}`,
			expectedPorts: 1,
			checkFunc: func(t *testing.T, status FunnelStatus) {
				if !status.AllowFunnel["myhost.tail1234.ts.net:443"] {
					t.Error("expected AllowFunnel to be true for port 443")
				}
				tcpCfg, ok := status.TCP["443"]
				if !ok {
					t.Fatal("expected TCP config for port 443")
				}
				if !tcpCfg["HTTPS"] {
					t.Error("expected HTTPS=true in TCP config")
				}
			},
		},
		{
			name: "multiple funnel ports",
			input: `{
				"TCP": {},
				"Web": {},
				"AllowFunnel": {
					"myhost.tail1234.ts.net:443": true,
					"myhost.tail1234.ts.net:8443": true
				}
			}`,
			expectedPorts: 2,
		},
		{
			name: "no funnels",
			input: `{
				"TCP": {},
				"Web": {},
				"AllowFunnel": {}
			}`,
			expectedPorts: 0,
		},
		{
			name:          "empty JSON",
			input:         `{}`,
			expectedPorts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var status FunnelStatus
			if err := json.Unmarshal([]byte(tt.input), &status); err != nil {
				t.Fatalf("failed to unmarshal: %v", err)
			}
			if len(status.AllowFunnel) != tt.expectedPorts {
				t.Errorf("expected %d funnel ports, got %d", tt.expectedPorts, len(status.AllowFunnel))
			}
			if tt.checkFunc != nil {
				tt.checkFunc(t, status)
			}
		})
	}
}

func TestParseFunnelStatus(t *testing.T) {
	status := FunnelStatus{
		TCP: map[string]map[string]bool{
			"443":  {"HTTPS": true},
			"8443": {"HTTPS": true},
			"10000": {
				"TCP": true,
			},
		},
		Web: map[string]FunnelWebConfig{
			"https://myhost.tail1234.ts.net:443": {
				Handlers: map[string]FunnelHandler{
					"/":       {Proxy: "http://172.22.0.13:3000"},
					"/foobar": {Proxy: "http://172.22.0.14:4000"},
				},
			},
			"https://myhost.tail1234.ts.net:8443": {
				Handlers: map[string]FunnelHandler{
					"/hooks/github": {Proxy: "http://172.22.0.15:5000"},
				},
			},
		},
		AllowFunnel: map[string]bool{
			"myhost.tail1234.ts.net:443":   true,
			"myhost.tail1234.ts.net:8443":  true,
			"myhost.tail1234.ts.net:10000": true,
		},
	}

	funnels := parseFunnelStatus(status)

	tests := []struct {
		name        string
		key         string
		wantPort    string
		wantPath    string
		wantProto   string
		wantDest    string
		wantPresent bool
	}{
		{
			name:        "root handler",
			key:         "443|/",
			wantPort:    "443",
			wantPath:    "/",
			wantProto:   "https",
			wantDest:    "http://172.22.0.13:3000",
			wantPresent: true,
		},
		{
			name:        "path handler sharing port",
			key:         "443|/foobar",
			wantPort:    "443",
			wantPath:    "/foobar",
			wantProto:   "https",
			wantDest:    "http://172.22.0.14:4000",
			wantPresent: true,
		},
		{
			name:        "nested path handler",
			key:         "8443|/hooks/github",
			wantPort:    "8443",
			wantPath:    "/hooks/github",
			wantProto:   "https",
			wantDest:    "http://172.22.0.15:5000",
			wantPresent: true,
		},
		{
			name:        "tcp funnel remains keyed by port only",
			key:         "10000",
			wantPort:    "10000",
			wantProto:   "tcp",
			wantPresent: true,
		},
		{
			name:        "http path is not collapsed to port",
			key:         "443",
			wantPresent: false,
		},
		{
			name:        "no phantom root entry when only non-root handler exists",
			key:         "8443|/",
			wantPresent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := funnels[tt.key]
			if ok != tt.wantPresent {
				t.Fatalf("presence for key %q = %v, want %v", tt.key, ok, tt.wantPresent)
			}
			if !tt.wantPresent {
				return
			}
			if got.PublicPort != tt.wantPort {
				t.Errorf("PublicPort = %q, want %q", got.PublicPort, tt.wantPort)
			}
			if got.Path != tt.wantPath {
				t.Errorf("Path = %q, want %q", got.Path, tt.wantPath)
			}
			if got.Protocol != tt.wantProto {
				t.Errorf("Protocol = %q, want %q", got.Protocol, tt.wantProto)
			}
			if got.Destination != tt.wantDest {
				t.Errorf("Destination = %q, want %q", got.Destination, tt.wantDest)
			}
		})
	}
}

func TestCurrentFunnelMatchesDesired(t *testing.T) {
	desired := &apptypes.ContainerService{
		IPAddress:        "172.22.0.13",
		FunnelTargetPort: "3000",
		FunnelFunnelPort: "8443",
		FunnelProtocol:   "https",
		FunnelPath:       "/",
	}

	tests := []struct {
		name    string
		current CurrentFunnel
		want    bool
	}{
		{
			name: "matching https funnel",
			current: CurrentFunnel{
				PublicPort:  "8443",
				Path:        "/",
				Protocol:    "https",
				Destination: "http://172.22.0.13:3000",
			},
			want: true,
		},
		{
			name: "mismatched path",
			current: CurrentFunnel{
				PublicPort:  "8443",
				Path:        "/foo",
				Protocol:    "https",
				Destination: "http://172.22.0.13:3000",
			},
			want: false,
		},
		{
			name: "mismatched destination",
			current: CurrentFunnel{
				PublicPort:  "8443",
				Path:        "/",
				Protocol:    "https",
				Destination: "http://172.22.0.13:8080",
			},
			want: false,
		},
		{
			name: "mismatched protocol",
			current: CurrentFunnel{
				PublicPort:  "8443",
				Protocol:    "tcp",
				Destination: "tcp://172.22.0.13:3000",
			},
			want: false,
		},
		{
			name: "unknown destination but matching protocol",
			current: CurrentFunnel{
				PublicPort: "8443",
				Protocol:   "https",
			},
			want: true,
		},
		{
			name: "missing state details forces reapply",
			current: CurrentFunnel{
				PublicPort: "8443",
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := currentFunnelMatchesDesired(tt.current, desired); got != tt.want {
				t.Errorf("currentFunnelMatchesDesired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectFunnelProtocol(t *testing.T) {
	tests := []struct {
		name   string
		input  map[string]bool
		expect string
	}{
		{
			name:   "https config",
			input:  map[string]bool{"HTTPS": true},
			expect: "https",
		},
		{
			name:   "tcp config",
			input:  map[string]bool{"TCP": true},
			expect: "tcp",
		},
		{
			name:   "tls terminated tcp config",
			input:  map[string]bool{"TLS_TERMINATED_TCP": true},
			expect: "tls-terminated-tcp",
		},
		{
			name:   "empty config",
			input:  map[string]bool{},
			expect: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectFunnelProtocol(tt.input); got != tt.expect {
				t.Errorf("detectFunnelProtocol() = %q, want %q", got, tt.expect)
			}
		})
	}
}
