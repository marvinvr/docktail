package docker

import (
	"strconv"
	"testing"
)

func TestResolveProtocols(t *testing.T) {
	tests := []struct {
		name                    string
		containerID             string
		targetPort              string
		servicePort             string
		serviceProtocol         string
		protocol                string
		expectedProtocol        string
		expectedServicePort     string
		expectedServiceProtocol string
		expectError             bool
	}{
		{
			name:                    "all empty defaults to http/80/http",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "",
			serviceProtocol:         "",
			protocol:                "",
			expectedProtocol:        "http",
			expectedServicePort:     "80",
			expectedServiceProtocol: "http",
		},
		{
			name:                    "container port 443 defaults protocol to https",
			containerID:             "abcdef123456",
			targetPort:              "443",
			servicePort:             "",
			serviceProtocol:         "",
			protocol:                "",
			expectedProtocol:        "https",
			expectedServicePort:     "80",
			expectedServiceProtocol: "http",
		},
		{
			name:                    "tcp backend defaults service to tcp",
			containerID:             "abcdef123456",
			targetPort:              "5432",
			servicePort:             "",
			serviceProtocol:         "",
			protocol:                "tcp",
			expectedProtocol:        "tcp",
			expectedServicePort:     "80",
			expectedServiceProtocol: "tcp",
		},
		{
			name:                    "tls-terminated-tcp backend defaults service to same",
			containerID:             "abcdef123456",
			targetPort:              "5432",
			servicePort:             "",
			serviceProtocol:         "",
			protocol:                "tls-terminated-tcp",
			expectedProtocol:        "tls-terminated-tcp",
			expectedServicePort:     "80",
			expectedServiceProtocol: "tls-terminated-tcp",
		},
		{
			name:                    "service-protocol set, port unset: https defaults to 443",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "",
			serviceProtocol:         "https",
			protocol:                "",
			expectedProtocol:        "http",
			expectedServicePort:     "443",
			expectedServiceProtocol: "https",
		},
		{
			name:                    "service-protocol set, port unset: http defaults to 80",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "",
			serviceProtocol:         "http",
			protocol:                "",
			expectedProtocol:        "http",
			expectedServicePort:     "80",
			expectedServiceProtocol: "http",
		},
		{
			name:                    "service-port set, protocol unset: 443 defaults to https",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "443",
			serviceProtocol:         "",
			protocol:                "",
			expectedProtocol:        "http",
			expectedServicePort:     "443",
			expectedServiceProtocol: "https",
		},
		{
			name:                    "service-port set, protocol unset: 80 defaults to http",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "80",
			serviceProtocol:         "",
			protocol:                "",
			expectedProtocol:        "http",
			expectedServicePort:     "80",
			expectedServiceProtocol: "http",
		},
		{
			name:                    "service-port set to non-standard, protocol unset: defaults to http",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "3000",
			serviceProtocol:         "",
			protocol:                "",
			expectedProtocol:        "http",
			expectedServicePort:     "3000",
			expectedServiceProtocol: "http",
		},
		{
			name:                    "service-port with tcp backend defaults to tcp protocol",
			containerID:             "abcdef123456",
			targetPort:              "5432",
			servicePort:             "5432",
			serviceProtocol:         "",
			protocol:                "tcp",
			expectedProtocol:        "tcp",
			expectedServicePort:     "5432",
			expectedServiceProtocol: "tcp",
		},
		{
			name:                    "both explicitly set",
			containerID:             "abcdef123456",
			targetPort:              "8080",
			servicePort:             "443",
			serviceProtocol:         "https",
			protocol:                "http",
			expectedProtocol:        "http",
			expectedServicePort:     "443",
			expectedServiceProtocol: "https",
		},
		{
			name:        "invalid target protocol",
			containerID: "abcdef123456",
			targetPort:  "8080",
			protocol:    "invalid",
			expectError: true,
		},
		{
			name:            "invalid service protocol",
			containerID:     "abcdef123456",
			targetPort:      "8080",
			servicePort:     "443",
			serviceProtocol: "invalid",
			protocol:        "http",
			expectError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protocol, servicePort, serviceProtocol, err := resolveProtocols(
				tt.containerID, tt.targetPort, tt.servicePort, tt.serviceProtocol, tt.protocol,
			)

			if tt.expectError {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if protocol != tt.expectedProtocol {
				t.Errorf("protocol = %q, want %q", protocol, tt.expectedProtocol)
			}
			if servicePort != tt.expectedServicePort {
				t.Errorf("servicePort = %q, want %q", servicePort, tt.expectedServicePort)
			}
			if serviceProtocol != tt.expectedServiceProtocol {
				t.Errorf("serviceProtocol = %q, want %q", serviceProtocol, tt.expectedServiceProtocol)
			}
		})
	}
}

func TestIndexedPortRegex(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		shouldMatch   bool
		expectedIndex string
	}{
		{"standard indexed port", "docktail.service.1.port", true, "1"},
		{"higher index", "docktail.service.42.port", true, "42"},
		{"zero index", "docktail.service.0.port", true, "0"},
		{"primary port label", "docktail.service.port", false, ""},
		{"indexed service-port", "docktail.service.1.service-port", false, ""},
		{"indexed protocol", "docktail.service.1.protocol", false, ""},
		{"non-numeric index", "docktail.service.abc.port", false, ""},
		{"empty index", "docktail.service..port", false, ""},
		{"enable label", "docktail.service.enable", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := indexedPortRegex.FindStringSubmatch(tt.input)
			if tt.shouldMatch {
				if matches == nil {
					t.Fatal("expected match but got nil")
				}
				if matches[1] != tt.expectedIndex {
					t.Errorf("captured index = %q, want %q", matches[1], tt.expectedIndex)
				}
			} else {
				if matches != nil {
					t.Errorf("expected no match but got %v", matches)
				}
			}
		})
	}
}

func TestDuplicateServicePortDetection(t *testing.T) {
	// Exercises the dedup logic used in parseIndexedPorts: duplicates are
	// scoped by service name + port, so different services can use the same port.
	tests := []struct {
		name               string
		primaryServiceName string
		primaryServicePort string
		indexedPorts       []struct {
			index       int
			serviceName string
			servicePort string
		}
		expectedAccepted []int // indices that should pass dedup
		expectedSkipped  []int // indices that should be skipped
	}{
		{
			name:               "no duplicates, different names",
			primaryServiceName: "primary",
			primaryServicePort: "443",
			indexedPorts: []struct {
				index       int
				serviceName string
				servicePort string
			}{
				{1, "svc-a", "8080"},
				{2, "svc-b", "3000"},
			},
			expectedAccepted: []int{1, 2},
		},
		{
			name:               "same name and port as primary is skipped",
			primaryServiceName: "myapp",
			primaryServicePort: "80",
			indexedPorts: []struct {
				index       int
				serviceName string
				servicePort string
			}{
				{1, "myapp", "80"},
				{2, "other", "3000"},
			},
			expectedAccepted: []int{2},
			expectedSkipped:  []int{1},
		},
		{
			name:               "same port different names is allowed",
			primaryServiceName: "primary",
			primaryServicePort: "443",
			indexedPorts: []struct {
				index       int
				serviceName string
				servicePort string
			}{
				{1, "svc-a", "443"},
				{2, "svc-b", "443"},
			},
			expectedAccepted: []int{1, 2},
		},
		{
			name:               "same name same port across indices is skipped",
			primaryServiceName: "primary",
			primaryServicePort: "443",
			indexedPorts: []struct {
				index       int
				serviceName string
				servicePort string
			}{
				{1, "svc-a", "8080"},
				{2, "svc-a", "8080"},
			},
			expectedAccepted: []int{1},
			expectedSkipped:  []int{2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usedServicePorts := map[string]int{
				tt.primaryServiceName + ":" + tt.primaryServicePort: 0,
			}
			var accepted, skipped []int

			for _, ip := range tt.indexedPorts {
				dedupKey := ip.serviceName + ":" + ip.servicePort
				if _, exists := usedServicePorts[dedupKey]; exists {
					skipped = append(skipped, ip.index)
					continue
				}
				usedServicePorts[dedupKey] = ip.index
				accepted = append(accepted, ip.index)
			}

			if len(accepted) != len(tt.expectedAccepted) {
				t.Errorf("accepted = %v, want %v", accepted, tt.expectedAccepted)
			}
			for i, idx := range accepted {
				if i < len(tt.expectedAccepted) && idx != tt.expectedAccepted[i] {
					t.Errorf("accepted[%d] = %d, want %d", i, idx, tt.expectedAccepted[i])
				}
			}

			if len(skipped) != len(tt.expectedSkipped) {
				t.Errorf("skipped = %v, want %v", skipped, tt.expectedSkipped)
			}
			for i, idx := range skipped {
				if i < len(tt.expectedSkipped) && idx != tt.expectedSkipped[i] {
					t.Errorf("skipped[%d] = %d, want %d", i, idx, tt.expectedSkipped[i])
				}
			}
		})
	}
}

func TestCollectIndexedPorts(t *testing.T) {
	// Test that we correctly extract indices from label maps
	tests := []struct {
		name            string
		labels          map[string]string
		expectedIndices []int
	}{
		{
			name:            "no indexed labels",
			labels:          map[string]string{"docktail.service.port": "8080"},
			expectedIndices: nil,
		},
		{
			name: "single indexed port",
			labels: map[string]string{
				"docktail.service.1.port": "3000",
			},
			expectedIndices: []int{1},
		},
		{
			name: "multiple indexed ports",
			labels: map[string]string{
				"docktail.service.1.port": "3000",
				"docktail.service.2.port": "5432",
			},
			expectedIndices: []int{1, 2},
		},
		{
			name: "non-contiguous indices",
			labels: map[string]string{
				"docktail.service.1.port": "3000",
				"docktail.service.5.port": "5432",
				"docktail.service.3.port": "6379",
			},
			expectedIndices: []int{1, 3, 5},
		},
		{
			name: "only related labels are counted",
			labels: map[string]string{
				"docktail.service.1.port":             "3000",
				"docktail.service.1.service-port":     "3000",
				"docktail.service.1.protocol":         "tcp",
				"docktail.service.1.service-protocol": "tcp",
			},
			expectedIndices: []int{1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			indices := map[int]bool{}
			for key := range tt.labels {
				if matches := indexedPortRegex.FindStringSubmatch(key); matches != nil {
					idx, err := strconv.Atoi(matches[1])
					if err != nil {
						continue
					}
					indices[idx] = true
				}
			}

			if tt.expectedIndices == nil {
				if len(indices) != 0 {
					t.Errorf("expected no indices, got %v", indices)
				}
				return
			}

			if len(indices) != len(tt.expectedIndices) {
				t.Errorf("expected %d indices, got %d", len(tt.expectedIndices), len(indices))
			}

			for _, expected := range tt.expectedIndices {
				if !indices[expected] {
					t.Errorf("expected index %d to be present", expected)
				}
			}
		})
	}
}
