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
							"5432": {}
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

func TestCurrentFunnelMatchesDesired(t *testing.T) {
	desired := &apptypes.ContainerService{
		IPAddress:        "172.22.0.13",
		FunnelTargetPort: "3000",
		FunnelFunnelPort: "8443",
		FunnelProtocol:   "https",
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
				Protocol:    "https",
				Destination: "http://172.22.0.13:3000",
			},
			want: true,
		},
		{
			name: "mismatched destination",
			current: CurrentFunnel{
				PublicPort:  "8443",
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
