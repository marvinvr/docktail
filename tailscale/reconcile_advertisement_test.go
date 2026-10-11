package tailscale

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apptypes "github.com/marvinvr/docktail/types"
)

// Exercise the real reconciliation/CLI boundary without a tailnet or daemon.
// The literal Serve fixture keeps the configured state independent of prefs.
const advertisementServeFixture = `{"Services":{"svc:web":{"TCP":{
	"443":{"TCPForward":"127.0.0.1:8080"},
	"9000":{"TCPForward":"127.0.0.1:9000"}
}}}}`

func fakeAdvertisementCLI(t *testing.T, prefs string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"status.json": advertisementServeFixture,
		"prefs.json":  prefs,
		"calls":       "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const script = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TS_DIR/calls"
case "$*" in
  version) printf '1.102.4\n' ;;
  'serve status --json') cat "$FAKE_TS_DIR/status.json" ;;
  'debug prefs')
    if [ "${FAKE_TS_PREFS_FAIL:-0}" = 1 ]; then
      echo 'private prefs diagnostic' >&2
      exit 1
    fi
    cat "$FAKE_TS_DIR/prefs.json"
    ;;
  'funnel status --json') printf '{}\n' ;;
  'serve drain '*)
    if [ "${FAKE_TS_DRAIN_BLOCK:-0}" = 1 ]; then exec sleep 5; fi
    ;;
  'serve advertise '*) exit "${FAKE_TS_ADVERTISE_FAIL:-0}" ;;
  'serve clear '*|'serve --service='*) ;;
  *) echo "Unexpected fake tailscale command: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake CLI in t.TempDir must be owner-executable.
		t.Fatal(err)
	}
	t.Setenv("FAKE_TS_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return filepath.Join(dir, "calls")
}

func advertisementCLICalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the test's call log in t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func advertisementDesiredServices() []*apptypes.ContainerService {
	return []*apptypes.ContainerService{
		{ContainerName: "web", ServiceEnabled: true, ServiceName: "web", Port: "443", ServiceProtocol: "tcp", Protocol: "tcp", IPAddress: "127.0.0.1", TargetPort: "8080"},
		{ContainerName: "metrics", ServiceEnabled: true, ServiceName: "web", Port: "9000", ServiceProtocol: "tcp", Protocol: "tcp", IPAddress: "127.0.0.1", TargetPort: "9000"},
	}
}

func TestReconcileAdvertisementDrift(t *testing.T) {
	for _, tt := range []struct {
		name          string
		prefs         string
		prefsFail     bool
		ignored       bool
		conflicted    bool
		advertiseFail bool
		configChanged bool
		wantAdvertise bool
		wantErr       bool
	}{
		{name: "missing advertisement repaired once for multiple ports", prefs: `{"AdvertiseServices":["svc:unrelated"]}`, wantAdvertise: true},
		{name: "already advertised is untouched", prefs: `{"AdvertiseServices":["svc:web"]}`},
		{name: "null is a known empty advertisement set", prefs: `{"AdvertiseServices":null}`, wantAdvertise: true},
		{name: "CLI warning before prefs JSON", prefs: "Warning: version mismatch\n" + `{"AdvertiseServices":[]}`, wantAdvertise: true},
		{name: "prefs failure is unknown", prefs: `{"AdvertiseServices":[]}`, prefsFail: true},
		{name: "malformed prefs are unknown", prefs: `{"AdvertiseServices":`},
		{name: "missing field is unknown", prefs: `{}`},
		{name: "invalid field type is unknown", prefs: `{"AdvertiseServices":"svc:web"}`},
		{name: "ignored service is untouched", prefs: `{"AdvertiseServices":[]}`, ignored: true},
		{name: "changed sibling uses config update without stale advertisement repair", prefs: `{"AdvertiseServices":[]}`, configChanged: true},
		{name: "conflicted service and sibling port are untouched", prefs: `{"AdvertiseServices":[]}`, conflicted: true, wantErr: true},
		{name: "advertisement failure is reported", prefs: `{"AdvertiseServices":[]}`, advertiseFail: true, wantAdvertise: true, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			callsPath := fakeAdvertisementCLI(t, tt.prefs)
			if tt.prefsFail {
				t.Setenv("FAKE_TS_PREFS_FAIL", "1")
			}
			if tt.advertiseFail {
				t.Setenv("FAKE_TS_ADVERTISE_FAIL", "1")
			}
			c := NewClient(ClientConfig{})
			if tt.ignored {
				c.ignoredServices["web"] = struct{}{}
			}
			desired := advertisementDesiredServices()
			if tt.configChanged {
				desired[1].TargetPort = "9001"
			}
			if tt.conflicted {
				duplicate := *desired[0]
				duplicate.ContainerName = "intruder"
				desired = append(desired, &duplicate)
			}
			err := c.ReconcileServices(context.Background(), desired)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReconcileServices error = %v, want error %v", err, tt.wantErr)
			}
			calls := advertisementCLICalls(t, callsPath)
			var mutations []string
			prefsReads := 0
			for _, call := range calls {
				if call == "debug prefs" {
					prefsReads++
				}
				if strings.HasPrefix(call, "serve ") && call != "serve status --json" {
					mutations = append(mutations, call)
				}
			}
			var want []string
			if tt.wantAdvertise {
				want = []string{"serve advertise svc:web"}
			}
			if tt.configChanged {
				want = []string{"serve --service=svc:web --tcp=9000 tcp://127.0.0.1:9001"}
			}
			if !slices.Equal(mutations, want) {
				t.Errorf("CLI mutations = %v, want %v (all calls: %v)", mutations, want, calls)
			}
			if prefsReads != 1 {
				t.Errorf("prefs reads = %d, want 1", prefsReads)
			}
		})
	}
}

func TestReconcileAdvertisementFollowsDefinitionSync(t *testing.T) {
	callsPath := fakeAdvertisementCLI(t, `{"AdvertiseServices":[]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/tailnet/example.com/services/svc:web" {
			t.Errorf("unexpected API path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case http.MethodPut:
			f, err := os.OpenFile(callsPath, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: callsPath is the test's call log in t.TempDir.
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if _, err := f.WriteString("control-plane definition synced\n"); err != nil {
				t.Error(err)
			}
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected API method: %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{})
	c.apiSyncEnabled = true
	c.tailnet = "example.com"
	c.baseURL = srv.URL
	c.httpClient = srv.Client()
	if err := c.ReconcileServices(context.Background(), advertisementDesiredServices()); err != nil {
		t.Fatal(err)
	}
	calls := advertisementCLICalls(t, callsPath)
	synced := slices.Index(calls, "control-plane definition synced")
	advertised := slices.Index(calls, "serve advertise svc:web")
	if synced == -1 || advertised <= synced {
		t.Fatalf("definition must be synced before re-advertising: %v", calls)
	}
}

func TestCleanupAllServicesDeduplicatesNames(t *testing.T) {
	callsPath := fakeAdvertisementCLI(t, `{"AdvertiseServices":["svc:web"]}`)
	if err := NewClient(ClientConfig{}).CleanupAllServices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := advertisementCLICalls(t, callsPath), []string{"funnel status --json", "serve status --json", "serve drain svc:web", "serve clear svc:web"}; !slices.Equal(got, want) {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
	}
}

func TestCleanupAllServicesStopsAfterDeadline(t *testing.T) {
	callsPath := fakeAdvertisementCLI(t, `{"AdvertiseServices":["svc:web"]}`)
	t.Setenv("FAKE_TS_DRAIN_BLOCK", "1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := NewClient(ClientConfig{}).CleanupAllServices(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup error = %v, want context deadline exceeded", err)
	}
	if got, want := advertisementCLICalls(t, callsPath), []string{"funnel status --json", "serve status --json", "serve drain svc:web"}; !slices.Equal(got, want) {
		t.Fatalf("cleanup must not clear or schedule another service after deadline: %v", got)
	}
}

func TestCleanupAllServicesAlreadyCanceled(t *testing.T) {
	callsPath := fakeAdvertisementCLI(t, `{"AdvertiseServices":[]}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewClient(ClientConfig{}).CleanupAllServices(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup error = %v, want context canceled", err)
	}
	if got := advertisementCLICalls(t, callsPath); len(got) != 0 {
		t.Fatalf("canceled cleanup must not schedule CLI commands: %v", got)
	}
}
