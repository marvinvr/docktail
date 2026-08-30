package tailscale

import "testing"

// A service with several ports can have a different backend per port. The
// representative backend recorded for the service must not depend on Go's map
// iteration order: the recorder writes a record every time the state string
// differs from the last one, so an unstable choice would fill the diagnostics
// file with changes that never happened.
func TestServiceStatesPicksAStableBackend(t *testing.T) {
	serveConfig := map[string]ServiceEndpoint{
		"svc:web:443": {ServiceName: "svc:web", Port: "443", Protocol: "https", Destination: "http://10.0.0.9:8443"},
		"svc:web:80":  {ServiceName: "svc:web", Port: "80", Protocol: "http", Destination: "http://10.0.0.2:8080"},
		"svc:web:8080": {ServiceName: "svc:web", Port: "8080", Protocol: "http",
			Destination: "http://10.0.0.7:9090"},
	}

	first := ServiceStates(serveConfig, []string{"svc:web"})
	if len(first) != 1 {
		t.Fatalf("expected one service, got %d", len(first))
	}

	for i := 0; i < 50; i++ {
		got := ServiceStates(serveConfig, []string{"svc:web"})
		if len(got) != 1 {
			t.Fatalf("iteration %d: expected one service, got %d", i, len(got))
		}
		if got[0].Destination != first[0].Destination {
			t.Fatalf("iteration %d: destination changed between samples of identical state: %q then %q",
				i, first[0].Destination, got[0].Destination)
		}
		if got[0].Protocol != first[0].Protocol {
			t.Fatalf("iteration %d: protocol changed between samples of identical state: %q then %q",
				i, first[0].Protocol, got[0].Protocol)
		}
		if VIPFingerprint(got) != VIPFingerprint(first) {
			t.Fatalf("iteration %d: fingerprint changed between samples of identical state", i)
		}
	}
}

// A service that is advertised but carries no serve config, and one that has
// serve config but is not advertised, are the two halves that must show up as
// anomalies — they are the whole reason the recorder exists.
func TestServiceStatesReportsBothHalves(t *testing.T) {
	serveConfig := map[string]ServiceEndpoint{
		"svc:configured-only:80": {ServiceName: "svc:configured-only", Port: "80", Protocol: "http", Destination: "http://10.0.0.2:80"},
	}

	states := ServiceStates(serveConfig, []string{"svc:advertised-only"})
	if len(states) != 2 {
		t.Fatalf("expected both services, got %d: %+v", len(states), states)
	}

	byName := map[string]ServiceState{}
	for _, s := range states {
		byName[s.Name] = s
	}

	configured := byName["svc:configured-only"]
	if !configured.Configured || configured.Advertised {
		t.Fatalf("expected configured-but-not-advertised, got %+v", configured)
	}
	if configured.Anomaly() == "" {
		t.Fatal("a configured but unadvertised service must be reported as an anomaly")
	}

	advertised := byName["svc:advertised-only"]
	if advertised.Configured || !advertised.Advertised {
		t.Fatalf("expected advertised-but-not-configured, got %+v", advertised)
	}
	if advertised.Anomaly() == "" {
		t.Fatal("an advertised service with no serve config must be reported as an anomaly")
	}
}
