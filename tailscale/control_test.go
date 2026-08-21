package tailscale

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServiceControlStateDecodesAdvertisingHostNodeIDs(t *testing.T) {
	const response = `{
		"hosts": [
			{
				"nodeId": "nHealthyCNTRL",
				"approvalLevel": "approved:auto",
				"configured": "ready"
			},
			{
				"nodeId": "nNeedsConfigCNTRL",
				"approvalLevel": "approved:manual",
				"configured": "not-configured"
			}
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/api/v2/tailnet/example.com/services/svc:web/devices" {
			t.Errorf("path = %q, want service devices endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()

	c := &Client{
		baseURL:    srv.URL,
		tailnet:    "example.com",
		httpClient: srv.Client(),
	}
	got, err := c.ServiceControlState(context.Background(), "web")
	if err != nil {
		t.Fatalf("ServiceControlState returned error: %v", err)
	}
	if !got.Exists {
		t.Fatal("Exists = false, want true")
	}
	if got.ServiceName != "svc:web" {
		t.Fatalf("ServiceName = %q, want %q", got.ServiceName, "svc:web")
	}

	want := []ServiceControlHost{
		{
			NodeID:        "nHealthyCNTRL",
			State:         ControlStateConnected,
			ApprovalLevel: "approved:auto",
			Configured:    "ready",
		},
		{
			NodeID:        "nNeedsConfigCNTRL",
			State:         ControlStateNeedsConfig,
			ApprovalLevel: "approved:manual",
			Configured:    "not-configured",
		},
	}
	if len(got.Hosts) != len(want) {
		t.Fatalf("len(Hosts) = %d, want %d: %+v", len(got.Hosts), len(want), got.Hosts)
	}
	for i := range want {
		if got.Hosts[i] != want[i] {
			t.Errorf("Hosts[%d] = %+v, want %+v", i, got.Hosts[i], want[i])
		}
	}
}
