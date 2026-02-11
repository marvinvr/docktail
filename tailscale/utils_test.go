package tailscale

import (
	"testing"

	apptypes "github.com/marvinvr/docktail/types"
)

func TestStripWarnings(t *testing.T) {
	tests := []struct {
		name     string
		input    []byte
		expected string
	}{
		{
			name:     "no warnings - starts with JSON",
			input:    []byte(`{"Services":{}}`),
			expected: `{"Services":{}}`,
		},
		{
			name:     "warning before JSON",
			input:    []byte("Warning: some tailscale warning\n{\"Services\":{}}"),
			expected: `{"Services":{}}`,
		},
		{
			name:     "multiple warnings before JSON",
			input:    []byte("Warning: first\nWarning: second\n{\"key\":\"value\"}"),
			expected: `{"key":"value"}`,
		},
		{
			name:     "no JSON brace at all",
			input:    []byte("just a plain string"),
			expected: "just a plain string",
		},
		{
			name:     "empty input",
			input:    []byte(""),
			expected: "",
		},
		{
			name:     "brace at position 0 unchanged",
			input:    []byte("{\"already\":\"clean\"}"),
			expected: `{"already":"clean"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := stripWarnings(tt.input)
			if result != tt.expected {
				t.Errorf("stripWarnings() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestIsNotFoundError(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		expected bool
	}{
		{"contains not found", "error: service not found", true},
		{"contains does not exist", "error: resource does not exist", true},
		{"contains no services", "no services configured", true},
		{"contains nothing to show", "nothing to show", true},
		{"contains no funnel", "no funnel configured", true},
		{"unrelated error", "permission denied", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isNotFoundError(tt.stderr)
			if result != tt.expected {
				t.Errorf("isNotFoundError(%q) = %v, want %v", tt.stderr, result, tt.expected)
			}
		})
	}
}

func TestIsConfigConflictError(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		expected bool
	}{
		{"contains already serving", "error: port is already serving HTTPS", true},
		{"contains want to serve", "error: want to serve HTTP but already serving HTTPS", true},
		{"contains port is already serving", "port is already serving TCP", true},
		{"unrelated error", "connection refused", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isConfigConflictError(tt.stderr)
			if result != tt.expected {
				t.Errorf("isConfigConflictError(%q) = %v, want %v", tt.stderr, result, tt.expected)
			}
		})
	}
}

func TestIsUntaggedNodeError(t *testing.T) {
	tests := []struct {
		name     string
		stderr   string
		expected bool
	}{
		{"matching error", "error: service hosts must be tagged nodes", true},
		{"unrelated error", "permission denied", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isUntaggedNodeError(tt.stderr)
			if result != tt.expected {
				t.Errorf("isUntaggedNodeError(%q) = %v, want %v", tt.stderr, result, tt.expected)
			}
		})
	}
}

func TestIsManagedService(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		expected    bool
	}{
		{"has svc prefix", "svc:web", true},
		{"has svc prefix with complex name", "svc:my-service-123", true},
		{"no prefix", "web", false},
		{"empty string", "", false},
		{"partial prefix", "sv:web", false},
		{"svc without colon", "svcweb", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isManagedService(tt.serviceName)
			if result != tt.expected {
				t.Errorf("isManagedService(%q) = %v, want %v", tt.serviceName, result, tt.expected)
			}
		})
	}
}

func TestBuildDestination(t *testing.T) {
	tests := []struct {
		name     string
		svc      *apptypes.ContainerService
		expected string
	}{
		{
			name: "HTTP service",
			svc: &apptypes.ContainerService{
				Protocol:   "http",
				IPAddress:  "172.17.0.2",
				TargetPort: "8080",
			},
			expected: "http://172.17.0.2:8080",
		},
		{
			name: "HTTPS service",
			svc: &apptypes.ContainerService{
				Protocol:   "https",
				IPAddress:  "172.17.0.3",
				TargetPort: "443",
			},
			expected: "https://172.17.0.3:443",
		},
		{
			name: "TCP service",
			svc: &apptypes.ContainerService{
				Protocol:   "tcp",
				IPAddress:  "10.0.0.5",
				TargetPort: "5432",
			},
			expected: "tcp://10.0.0.5:5432",
		},
		{
			name: "localhost destination",
			svc: &apptypes.ContainerService{
				Protocol:   "http",
				IPAddress:  "localhost",
				TargetPort: "9080",
			},
			expected: "http://localhost:9080",
		},
		{
			name: "https+insecure protocol",
			svc: &apptypes.ContainerService{
				Protocol:   "https+insecure",
				IPAddress:  "172.17.0.4",
				TargetPort: "8443",
			},
			expected: "https+insecure://172.17.0.4:8443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildDestination(tt.svc)
			if result != tt.expected {
				t.Errorf("buildDestination() = %q, want %q", result, tt.expected)
			}
		})
	}
}
