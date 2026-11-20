package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

func TestRedirectTarget(t *testing.T) {
	for _, tt := range []struct{ name, port, want string }{
		{"web", "443", "308:https://web.tail1234.ts.net${REQUEST_URI}"},
		{"web", "", "308:https://web.tail1234.ts.net${REQUEST_URI}"},
		{"api", "8443", "308:https://api.tail1234.ts.net:8443${REQUEST_URI}"},
	} {
		if got := redirectTarget(tt.name, tt.port, "tail1234.ts.net"); got != tt.want {
			t.Errorf("redirectTarget(%q, %q) = %q, want %q", tt.name, tt.port, got, tt.want)
		}
	}
}

func TestSupportsServeRedirect(t *testing.T) {
	for version, want := range map[string]bool{
		"1.104.4-t1234abcd-g5678": true,
		"1.92.0":                  true,
		"1.92":                    true,
		"1.90.9":                  false,
		"1.88.3-tabc":             false,
		"2.0.0":                   true,
		"0.99.0":                  false,
		"":                        true, // unknown: do not disable the feature
		"dev-build":               true,
	} {
		if got := supportsServeRedirect(version); got != want {
			t.Errorf("supportsServeRedirect(%q) = %v, want %v", version, got, want)
		}
	}
}

func redirectHTTPSService() *apptypes.ContainerService {
	return &apptypes.ContainerService{
		ContainerName: "web", ServiceEnabled: true, ServiceName: "web", Port: "443",
		ServiceProtocol: "https", ServicePath: "/", Protocol: "http", IPAddress: "172.17.0.2", TargetPort: "8080",
		HTTPRedirect: true, Tags: []string{"tag:container"},
	}
}

func TestExpandHTTPRedirects(t *testing.T) {
	plain := &apptypes.ContainerService{ContainerName: "db", ServiceEnabled: true, ServiceName: "db", Port: "5432", ServiceProtocol: "tcp"}
	desired := []*apptypes.ContainerService{redirectHTTPSService(), plain}

	got, skipped := expandHTTPRedirects(desired, "tail1234.ts.net", nil)
	if len(desired) != 2 {
		t.Fatal("expandHTTPRedirects must not modify its input slice")
	}
	if len(got) != 3 || len(skipped) != 0 {
		t.Fatalf("got %d services (skipped %v), want 3", len(got), skipped)
	}
	r := got[2]
	if r.ServiceName != "web" || r.Port != "80" || r.ServiceProtocol != "http" ||
		r.RedirectTo != "308:https://web.tail1234.ts.net${REQUEST_URI}" || r.HTTPRedirect {
		t.Errorf("derived redirect endpoint = %+v", r)
	}

	// New Service definitions get the redirect port, and existing ones are
	// required to allow it.
	defs := aggregateServiceDefinitions(got)
	if !slices.Equal(defs["web"].Ports, []string{"443", "80"}) {
		t.Errorf("web definition ports = %v, want [443 80]", defs["web"].Ports)
	}
	if !slices.Equal(defs["web"].RequiredPorts, []string{"80"}) {
		t.Errorf("web definition required ports = %v, want [80]", defs["web"].RequiredPorts)
	}
	if len(defs["db"].RequiredPorts) != 0 {
		t.Errorf("db definition required ports = %v, want none", defs["db"].RequiredPorts)
	}

	if out, _ := expandHTTPRedirects([]*apptypes.ContainerService{plain}, "tail1234.ts.net", nil); len(out) != 1 {
		t.Errorf("no redirect requested: got %d services, want 1", len(out))
	}

	optedOut := redirectHTTPSService()
	optedOut.HTTPRedirect = false
	if out, _ := expandHTTPRedirects([]*apptypes.ContainerService{optedOut}, "tail1234.ts.net", nil); len(out) != 1 {
		t.Errorf("opted out: got %d services, want 1", len(out))
	}
}

func TestExpandHTTPRedirectsNeverConflicts(t *testing.T) {
	const suffix = "tail1234.ts.net"
	httpsOn := func(container, port string) *apptypes.ContainerService {
		svc := redirectHTTPSService()
		svc.ContainerName, svc.Port = container, port
		return svc
	}

	t.Run("port 80 declared by another container", func(t *testing.T) {
		legacy := &apptypes.ContainerService{ContainerName: "legacy", ServiceEnabled: true, ServiceName: "web", Port: "80", ServiceProtocol: "http"}
		out, skipped := expandHTTPRedirects([]*apptypes.ContainerService{redirectHTTPSService(), legacy}, suffix, nil)
		if len(out) != 2 || !slices.Equal(skipped, []string{"web"}) {
			t.Fatalf("got %d services, skipped %v; want 2 and [web]", len(out), skipped)
		}
		if _, conflicts := buildDesiredServiceMap(out); len(conflicts) != 0 {
			t.Errorf("conflicts = %v, want none", conflicts)
		}
	})

	t.Run("port 80 declared by the same container", func(t *testing.T) {
		own := &apptypes.ContainerService{ContainerName: "web", ServiceEnabled: true, ServiceName: "web", Port: "80", ServiceProtocol: "http"}
		out, skipped := expandHTTPRedirects([]*apptypes.ContainerService{redirectHTTPSService(), own}, suffix, nil)
		if len(out) != 2 || !slices.Equal(skipped, []string{"web"}) {
			t.Fatalf("got %d services, skipped %v; want 2 and [web]", len(out), skipped)
		}
	})

	t.Run("disabled port 80 does not block", func(t *testing.T) {
		off := &apptypes.ContainerService{ContainerName: "legacy", ServiceEnabled: false, ServiceName: "web", Port: "80", ServiceProtocol: "http"}
		if out, skipped := expandHTTPRedirects([]*apptypes.ContainerService{redirectHTTPSService(), off}, suffix, nil); len(out) != 3 || len(skipped) != 0 {
			t.Fatalf("got %d services, skipped %v; want 3 and none", len(out), skipped)
		}
	})

	t.Run("ignored service", func(t *testing.T) {
		ignored := func(name string) bool { return name == "web" }
		out, skipped := expandHTTPRedirects([]*apptypes.ContainerService{redirectHTTPSService()}, suffix, ignored)
		if len(out) != 1 || !slices.Equal(skipped, []string{"web"}) {
			t.Fatalf("got %d services, skipped %v; want 1 and [web]", len(out), skipped)
		}
	})

	t.Run("several HTTPS ports share one redirect aimed at 443", func(t *testing.T) {
		for _, order := range [][]*apptypes.ContainerService{
			{httpsOn("web", "8443"), httpsOn("web", "443")},
			{httpsOn("web", "443"), httpsOn("web", "8443")},
		} {
			out, _ := expandHTTPRedirects(order, suffix, nil)
			if len(out) != 3 {
				t.Fatalf("got %d services, want 3", len(out))
			}
			if got := out[2].RedirectTo; got != "308:https://web.tail1234.ts.net${REQUEST_URI}" {
				t.Errorf("redirect target = %q, want port 443", got)
			}
			if _, conflicts := buildDesiredServiceMap(out); len(conflicts) != 0 {
				t.Errorf("conflicts = %v, want none", conflicts)
			}
		}
	})

	t.Run("several containers share one redirect at the lowest port", func(t *testing.T) {
		for _, order := range [][]*apptypes.ContainerService{
			{httpsOn("b", "9443"), httpsOn("a", "8443")},
			{httpsOn("a", "8443"), httpsOn("b", "9443")},
		} {
			out, _ := expandHTTPRedirects(order, suffix, nil)
			if len(out) != 3 {
				t.Fatalf("got %d services, want 3", len(out))
			}
			r := out[2]
			if r.ContainerName != "a" || r.RedirectTo != "308:https://web.tail1234.ts.net:8443${REQUEST_URI}" {
				t.Errorf("redirect = %s %q, want a -> :8443", r.ContainerName, r.RedirectTo)
			}
			if _, conflicts := buildDesiredServiceMap(out); len(conflicts) != 0 {
				t.Errorf("conflicts = %v, want none", conflicts)
			}
		}
	})
}

// expandHTTPRedirectsAll is expandHTTPRedirects without an ignore list.
func expandHTTPRedirectsAll(desired []*apptypes.ContainerService, suffix string) []*apptypes.ContainerService {
	out, _ := expandHTTPRedirects(desired, suffix, nil)
	return out
}

func TestDefinitionAllowsPort(t *testing.T) {
	tests := []struct {
		ports []string
		want  bool
	}{
		{nil, false},
		{[]string{"tcp:443"}, false},
		{[]string{"tcp:443", "tcp:80"}, true},
		{[]string{"tcp:1-1024"}, true},
		{[]string{"tcp:81-90"}, false},
		{[]string{"tcp:*"}, true},
		{[]string{"udp:80"}, false},
	}
	for _, tt := range tests {
		if got := definitionAllowsPort(tt.ports, "80"); got != tt.want {
			t.Errorf("definitionAllowsPort(%v, 80) = %v, want %v", tt.ports, got, tt.want)
		}
	}
}

func TestParseManagedServicesRedirect(t *testing.T) {
	var status TailscaleStatus
	if err := json.Unmarshal([]byte(`{"Services":{"svc:web":{
		"TCP":{"443":{"HTTPS":true},"80":{"HTTP":true}},
		"Web":{
			"web.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":"http://172.17.0.2:8080"}}},
			"web.tail1234.ts.net:80":{"Handlers":{"/":{"Redirect":"308:https://web.tail1234.ts.net${REQUEST_URI}"}}}
		}}}}`), &status); err != nil {
		t.Fatal(err)
	}
	services := parseManagedServices(status)
	r := services["svc:web:80"]
	if r.Protocol != "http" || r.Destination != "" || r.Path != "/" || r.Redirect != "308:https://web.tail1234.ts.net${REQUEST_URI}" {
		t.Errorf("redirect endpoint = %+v", r)
	}
	if h := services["svc:web:443"]; h.Destination != "http://172.17.0.2:8080" || h.Redirect != "" {
		t.Errorf("https endpoint = %+v", h)
	}

	desired := expandHTTPRedirectsAll([]*apptypes.ContainerService{redirectHTTPSService()}, "tail1234.ts.net")
	if serviceConfigChanged(r, desired[1]) {
		t.Error("matching redirect reported as changed")
	}
	if !serviceConfigChanged(r, redirectHTTPSService()) {
		t.Error("redirect replaced by a proxy must be reported as changed")
	}
	moved := expandHTTPRedirectsAll([]*apptypes.ContainerService{redirectHTTPSService()}, "other.ts.net")
	if !serviceConfigChanged(r, moved[1]) {
		t.Error("redirect with a different target must be reported as changed")
	}
}

// fakeLocalAPI serves tailscaled's /localapi/v0/serve-config on a Unix
// socket, keeping the config in serve.json so the fake CLI's
// `serve status --json` sees every write.
type fakeLocalAPI struct {
	dir      string
	mu       sync.Mutex
	version  int
	posts    int
	conflict int // number of POSTs to reject with 412 first
}

func (f *fakeLocalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := filepath.Join(f.dir, "serve.json")
	etag := func() string { return `"v` + strconv.Itoa(f.version) + `"` }
	switch r.Method {
	case http.MethodGet:
		data, _ := os.ReadFile(path) //nolint:gosec // G304: test fixture in t.TempDir.
		w.Header().Set("Etag", etag())
		_, _ = w.Write(data)
	case http.MethodPost:
		f.posts++
		if r.Header.Get("If-Match") != etag() || f.conflict > 0 {
			f.conflict--
			f.version++ // someone else wrote in between
			http.Error(w, "etag mismatch", http.StatusPreconditionFailed)
			return
		}
		data, _ := io.ReadAll(r.Body)
		_ = os.WriteFile(path, data, 0o600)
		f.version++
	}
}

func startFakeLocalAPI(t *testing.T, dir string) (*fakeLocalAPI, string) {
	t.Helper()
	// Unix socket paths are length-limited (104 bytes on macOS), and
	// t.TempDir can exceed that.
	sockDir, err := os.MkdirTemp("", "dt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeLocalAPI{dir: dir}
	srv := &http.Server{Handler: api} //nolint:gosec // G112: test server
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return api, sock
}

const redirectServeFixture = `{"Foreground":{"x":{"TCP":{"8080":{"HTTP":true}}}},"Services":{"svc:web":{
	"TCP":{"443":{"HTTPS":true}},
	"Web":{"web.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":"http://172.17.0.2:8080"}}}},
	"FutureField":{"keep":1.5e3}
}}}`

func fakeRedirectCLI(t *testing.T, tailscaledVersion string) (dir, callsPath string) {
	t.Helper()
	dir = t.TempDir()
	statusJSON := `{"Version":"` + tailscaledVersion + `","Self":{"ID":"n1","DNSName":"box.tail1234.ts.net."},"CurrentTailnet":{"Name":"example.com","MagicDNSSuffix":"tail1234.ts.net"}}`
	for name, content := range map[string]string{
		"serve.json":  redirectServeFixture,
		"status.json": statusJSON,
		"calls":       "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const script = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_TS_DIR/calls"
case "$*" in
  version) printf '1.104.4\n' ;;
  'serve status --json') cat "$FAKE_TS_DIR/serve.json" ;;
  'status --json') cat "$FAKE_TS_DIR/status.json" ;;
  'debug prefs') printf '{"AdvertiseServices":["svc:web"]}\n' ;;
  'funnel status --json') printf '{}\n' ;;
  'serve drain '*|'serve clear '*|'serve --service='*|'serve advertise '*) ;;
  *) echo "Unexpected fake tailscale command: $*" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the fake CLI in t.TempDir must be owner-executable.
		t.Fatal(err)
	}
	t.Setenv("FAKE_TS_DIR", dir)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, filepath.Join(dir, "calls")
}

func readServeFixture(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "serve.json")) //nolint:gosec // G304: test fixture in t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func mutatingCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.HasPrefix(c, "serve --service=") || strings.HasPrefix(c, "serve clear") || strings.HasPrefix(c, "serve drain") {
			out = append(out, c)
		}
	}
	return out
}

func TestReconcileHTTPRedirect(t *testing.T) {
	dir, callsPath := fakeRedirectCLI(t, "1.104.4-tabc")
	api, sock := startFakeLocalAPI(t, dir)
	api.conflict = 1 // the first write races with another writer
	c := NewClient(ClientConfig{SocketPath: sock})
	ctx := context.Background()

	// 1. Enabling the redirect writes only port 80, through the LocalAPI,
	// and keeps every field DockTail does not model.
	if err := c.ReconcileServices(ctx, []*apptypes.ContainerService{redirectHTTPSService()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := mutatingCalls(advertisementCLICalls(t, callsPath)); len(got) != 0 {
		t.Errorf("unchanged HTTPS endpoint was rewritten through the CLI: %v", got)
	}
	if api.posts != 2 {
		t.Errorf("LocalAPI posts = %d, want 2 (one 412, one retry)", api.posts)
	}
	cfg := readServeFixture(t, dir)
	svc := cfg["Services"].(map[string]any)["svc:web"].(map[string]any)
	if tcp80, _ := svc["TCP"].(map[string]any)["80"].(map[string]any); tcp80["HTTP"] != true {
		t.Errorf("TCP[80] = %v, want HTTP:true", tcp80)
	}
	handler := svc["Web"].(map[string]any)["web.tail1234.ts.net:80"].(map[string]any)["Handlers"].(map[string]any)["/"].(map[string]any)
	if handler["Redirect"] != "308:https://web.tail1234.ts.net${REQUEST_URI}" {
		t.Errorf("redirect handler = %v", handler)
	}
	if svc["FutureField"].(map[string]any)["keep"] != 1500.0 || cfg["Foreground"] == nil {
		t.Errorf("unmodelled fields were not preserved: %v", cfg)
	}

	// 2. A second cycle finds everything in place and writes nothing.
	postsBefore := api.posts
	if err := c.ReconcileServices(ctx, []*apptypes.ContainerService{redirectHTTPSService()}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if api.posts != postsBefore {
		t.Errorf("steady state wrote the serve config again (%d posts)", api.posts-postsBefore)
	}

	// 3. Turning the label off removes port 80 only: no drain or clear of the
	// service, which keeps serving HTTPS.
	off := redirectHTTPSService()
	off.HTTPRedirect = false
	if err := c.ReconcileServices(ctx, []*apptypes.ContainerService{off}); err != nil {
		t.Fatalf("reconcile without redirect: %v", err)
	}
	if got := mutatingCalls(advertisementCLICalls(t, callsPath)); len(got) != 0 {
		t.Errorf("removing the redirect touched the service through the CLI: %v", got)
	}
	svc = readServeFixture(t, dir)["Services"].(map[string]any)["svc:web"].(map[string]any)
	if _, ok := svc["TCP"].(map[string]any)["80"]; ok {
		t.Error("TCP[80] still present after disabling the redirect")
	}
	if _, ok := svc["Web"].(map[string]any)["web.tail1234.ts.net:80"]; ok {
		t.Error("redirect web handler still present after disabling the redirect")
	}
	if _, ok := svc["TCP"].(map[string]any)["443"]; !ok {
		t.Error("HTTPS endpoint was removed with the redirect")
	}
}

func TestReconcileHTTPRedirectOldTailscaled(t *testing.T) {
	dir, _ := fakeRedirectCLI(t, "1.90.1-tabc")
	api, sock := startFakeLocalAPI(t, dir)
	c := NewClient(ClientConfig{SocketPath: sock})
	if err := c.ReconcileServices(context.Background(), []*apptypes.ContainerService{redirectHTTPSService()}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if api.posts != 0 {
		t.Errorf("wrote a redirect to a tailscaled that cannot serve it (%d posts)", api.posts)
	}
}

func TestSyncServiceDefinitionAddsRedirectPort(t *testing.T) {
	tests := []struct {
		name      string
		existing  []string
		wantPut   bool
		wantPorts []string
	}{
		{name: "missing tcp:80 is appended", existing: []string{"tcp:443"}, wantPut: true, wantPorts: []string{"tcp:443", "tcp:80"}},
		{name: "allowed by a range", existing: []string{"tcp:1-1024"}},
		{name: "already listed", existing: []string{"tcp:443", "tcp:80"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var put *servicePutPayload
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(apiService{Name: "svc:web", Addrs: []string{"100.100.1.1"}, Tags: []string{"tag:container"}, Ports: tt.existing, Comment: "manual"})
				case http.MethodPut:
					var payload servicePutPayload
					_ = json.NewDecoder(r.Body).Decode(&payload)
					put = &payload
				}
			}))
			defer srv.Close()
			c := &Client{baseURL: srv.URL, tailnet: "example.com", httpClient: srv.Client(), apiSyncEnabled: true}

			defs := aggregateServiceDefinitions(expandHTTPRedirectsAll([]*apptypes.ContainerService{redirectHTTPSService()}, "tail1234.ts.net"))
			if err := c.syncServiceDefinition(context.Background(), "web", defs["web"]); err != nil {
				t.Fatalf("sync: %v", err)
			}
			if !tt.wantPut {
				if put != nil {
					t.Fatalf("unexpected PUT %+v", *put)
				}
				return
			}
			if put == nil {
				t.Fatal("expected a PUT adding the redirect port")
			}
			if !slices.Equal(put.Ports, tt.wantPorts) || put.Comment != "manual" ||
				!slices.Equal(put.Tags, []string{"tag:container"}) || !slices.Equal(put.Addrs, []string{"100.100.1.1"}) {
				t.Errorf("PUT = %+v, want ports %v and everything else kept", *put, tt.wantPorts)
			}
		})
	}
}

// captureLogs routes the global logger into a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = previous })
	return &buf
}

func TestReconcileHTTPRedirectSkipsTakenPort80(t *testing.T) {
	dir, _ := fakeRedirectCLI(t, "1.104.4-tabc")
	api, sock := startFakeLocalAPI(t, dir)
	logs := captureLogs(t)
	c := NewClient(ClientConfig{SocketPath: sock})
	legacy := &apptypes.ContainerService{
		ContainerName: "legacy", ServiceEnabled: true, ServiceName: "web", Port: "80",
		ServiceProtocol: "http", Protocol: "http", IPAddress: "172.17.0.3", TargetPort: "80",
	}
	for i := 0; i < 2; i++ {
		if err := c.ReconcileServices(context.Background(), []*apptypes.ContainerService{redirectHTTPSService(), legacy}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if api.posts != 0 {
		t.Errorf("wrote a redirect over a declared port 80 (%d posts)", api.posts)
	}
	if n := strings.Count(logs.String(), "HTTP redirect skipped"); n != 1 {
		t.Errorf("skip logged %d times, want once:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "conflict") {
		t.Errorf("a skipped redirect reported a conflict:\n%s", logs)
	}
}

func TestReconcileHTTPRedirectWarnsOnce(t *testing.T) {
	t.Run("old tailscaled", func(t *testing.T) {
		dir, _ := fakeRedirectCLI(t, "1.90.1-tabc")
		_, sock := startFakeLocalAPI(t, dir)
		logs := captureLogs(t)
		c := NewClient(ClientConfig{SocketPath: sock})
		for i := 0; i < 3; i++ {
			if err := c.ReconcileServices(context.Background(), []*apptypes.ContainerService{redirectHTTPSService()}); err != nil {
				t.Fatalf("reconcile %d: %v", i, err)
			}
		}
		if n := strings.Count(logs.String(), "cannot serve redirects"); n != 1 {
			t.Errorf("old tailscaled warning logged %d times, want once:\n%s", n, logs)
		}
	})

	t.Run("no API credentials", func(t *testing.T) {
		dir, _ := fakeRedirectCLI(t, "1.104.4-tabc")
		_, sock := startFakeLocalAPI(t, dir)
		logs := captureLogs(t)
		c := NewClient(ClientConfig{SocketPath: sock})
		for i := 0; i < 3; i++ {
			if err := c.ReconcileServices(context.Background(), []*apptypes.ContainerService{redirectHTTPSService()}); err != nil {
				t.Fatalf("reconcile %d: %v", i, err)
			}
		}
		if n := strings.Count(logs.String(), "cannot add tcp:80 to the Service definition"); n != 1 {
			t.Errorf("missing-credentials warning logged %d times, want once:\n%s", n, logs)
		}
	})
}
