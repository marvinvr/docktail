package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/cloud/proto"
	"github.com/marvinvr/docktail/health"
)

// Exercise the actual startup ordering with local Docker and Cloud fixtures.
// No Docker engine, tailscaled, or external Cloud connection is used.
func TestCloudConnectsDuringSocketWait(t *testing.T) {
	for _, scenario := range []string{"cancel wait", "ready with cleanup", "ready without cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			unmonitored := scenario != "ready without cleanup"
			var connections atomic.Int32
			frames := make(chan proto.Envelope, 32)
			cloudClosed := make(chan struct{}, 4)
			cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				connections.Add(1)
				defer func() { _ = ws.Close(); cloudClosed <- struct{}{} }()
				var hello proto.Envelope
				if err := ws.ReadJSON(&hello); err != nil {
					t.Error(err)
					return
				}
				frames <- hello
				// Config deliberately arrives before any reconcile, and before
				// hello_ack starts discovery. It must remain effective afterwards.
				for _, msg := range []struct {
					kind proto.MessageType
					body any
				}{
					{proto.TypeConfig, proto.Config{Version: 7, Unmonitored: unmonitored}},
					{proto.TypeHelloAck, proto.HelloAck{Accepted: true, HostID: "test-host", ConfigVersion: 7}},
				} {
					data, err := proto.Encode(msg.kind, time.Now().UnixMilli(), msg.body)
					if err != nil {
						t.Error(err)
						return
					}
					if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
						t.Error(err)
						return
					}
				}
				for {
					var frame proto.Envelope
					if err := ws.ReadJSON(&frame); err != nil {
						return
					}
					frames <- frame
				}
			}))
			t.Cleanup(cloudServer.Close)

			skipCleanup := scenario == "ready without cleanup"
			logs, socket, healthFile, cancel, done := startTestAgent(t, "dtc_0123456789abcdef", "ws"+strings.TrimPrefix(cloudServer.URL, "http"), skipCleanup)
			logs.wait(t, "Waiting for the Tailscale socket to become reachable")
			logs.wait(t, "cloud: applied config")
			// A full snapshot proves reporting works while startup is blocked.
			partialSnapshots := 0
			for {
				frame := receiveStartup(t, frames)
				if frame.Type == proto.TypeHello {
					var hello proto.Hello
					if err := frame.Decode(&hello); err != nil {
						t.Fatal(err)
					}
					if hello.Fingerprint != "test-engine" || hello.TailscaleNodeID != "" {
						t.Fatalf("unexpected hello while Tailscale is unavailable: %+v", hello)
					}
				}
				if frame.Type == proto.TypeSnapshot {
					var snapshot proto.Snapshot
					if err := frame.Decode(&snapshot); err != nil {
						t.Fatal(err)
					}
					if snapshot.Full {
						break
					}
					partialSnapshots++
				}
			}
			status := waitStartupHealth(t, healthFile)
			if status.Cloud == nil || (status.Cloud.State != health.CloudConnecting && status.Cloud.State != health.CloudConnected) || status.Reconcile.LastRunAt != nil || status.Tailscale == nil || status.Tailscale.SocketReachable {
				t.Fatalf("unexpected startup health: %+v", status)
			}
			if logs.contains("Starting reconciliation") || logs.contains("Starting cleanup:") {
				t.Fatal("reconcile or cleanup ran during the socket wait")
			}

			if scenario != "cancel wait" {
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						_ = conn.Close()
					}
				}()
				logs.wait(t, "Reconciliation completed successfully")
				if !unmonitored {
					// Observe delivery before canceling: shutdown can discard
					// queued frames, so a log alone is not a wire barrier.
					for partialSnapshots < 2 {
						frame := receiveStartup(t, frames)
						if frame.Type == proto.TypeSnapshot {
							var snapshot proto.Snapshot
							if err := frame.Decode(&snapshot); err != nil {
								t.Fatal(err)
							}
							if !snapshot.Full {
								partialSnapshots++
							}
						}
					}
				}
			}
			cancel()
			receiveStartup(t, done)
			receiveStartup(t, cloudClosed)
			if connections.Load() != 1 {
				t.Fatalf("Cloud started %d connections, want 1", connections.Load())
			}
			if _, err := os.Stat(healthFile); !os.IsNotExist(err) {
				t.Fatalf("health file still exists after shutdown: %v", err)
			}
			wantCleanup := scenario == "ready with cleanup"
			if logs.contains("Starting cleanup:") != wantCleanup {
				t.Fatalf("cleanup ran = %v, want %v", logs.contains("Starting cleanup:"), wantCleanup)
			}
			// Unmonitored config suppresses observer snapshots and inventory;
			// monitored config allows the first reconcile's observer snapshot.
			close(frames)
			for frame := range frames {
				if frame.Type == proto.TypeContainers && unmonitored {
					t.Fatal("Cloud config received before reconciliation was lost")
				}
				if frame.Type == proto.TypeSnapshot {
					var snapshot proto.Snapshot
					if err := frame.Decode(&snapshot); err != nil {
						t.Fatal(err)
					}
					if !snapshot.Full {
						partialSnapshots++
					}
				}
			}
			wantPartial := 1 // initial cached snapshot
			if !unmonitored {
				wantPartial++ // first successful reconcile
			}
			if partialSnapshots != wantPartial {
				t.Fatalf("partial snapshots = %d, want %d", partialSnapshots, wantPartial)
			}
		})
	}
}

func TestSocketWaitWithoutCloudConnection(t *testing.T) {
	for _, key := range []string{"", "invalid-key"} {
		t.Run(key, func(t *testing.T) {
			logs, _, healthFile, cancel, done := startTestAgent(t, key, "ws://127.0.0.1:1", false)
			logs.wait(t, "Waiting for the Tailscale socket to become reachable")
			status := waitStartupHealth(t, healthFile)
			if key == "" && status.Cloud != nil {
				t.Fatalf("disabled Cloud appears in health: %+v", status.Cloud)
			}
			if key != "" && (status.Cloud == nil || status.Cloud.State != health.CloudFailed) {
				t.Fatalf("initialization failure missing from health: %+v", status.Cloud)
			}
			cancel()
			receiveStartup(t, done)
			if logs.contains("Starting reconciliation") || logs.contains("Starting cleanup:") {
				t.Fatal("interrupted startup reconciled or cleaned up services")
			}
			if _, err := os.Stat(healthFile); !os.IsNotExist(err) {
				t.Fatalf("health file still exists after interrupted startup: %v", err)
			}
		})
	}
}

func startTestAgent(t *testing.T, key, cloudURL string, skipCleanup bool) (*startupLogs, string, string, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	dockerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/v1.47") {
		case "/info":
			_, _ = io.WriteString(w, `{"ID":"test-engine","Name":"test-host"}`)
		case "/version":
			_, _ = io.WriteString(w, `{"Version":"28.0.0"}`)
		case "/containers/json":
			_, _ = io.WriteString(w, `[]`)
		case "/events":
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(dockerServer.Close)
	for _, name := range []string{"TAILSCALE_API_KEY", "TAILSCALE_OAUTH_CLIENT_ID", "TAILSCALE_OAUTH_CLIENT_SECRET"} {
		clearSecretEnv(t, name)
	}
	// A short directory keeps Unix socket paths within macOS's length limit.
	dir, err := os.MkdirTemp("", "dt-start-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, healthFile := filepath.Join(dir, "ts.sock"), filepath.Join(dir, "health.json")
	const cli = `#!/bin/sh
case "$*" in
  'status --json') exit 1 ;;
  version) printf '1.102.4\n' ;;
  'serve status --json'|'funnel status --json'|'debug prefs') printf '{}\n' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tailscale"), []byte(cli), 0o700); err != nil { //nolint:gosec // G306: executable test fixture.
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"DOCKER_HOST": dockerServer.URL, "DOCKER_API_VERSION": "1.47",
		"DOCKER_TLS_VERIFY": "", "DOCKER_CERT_PATH": "",
		"TAILSCALE_SOCKET": socket, "HEALTH_FILE": healthFile,
		"PATH":               dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"DOCKTAIL_CLOUD_KEY": key, "DOCKTAIL_CLOUD_URL": cloudURL,
		"DOCKTAIL_CHECK_INTERVAL": "5m", "RECONCILE_INTERVAL": "1h",
		"UPDATE_CHECK": "false", "EXIT_ON_SOCKET_LOSS": "false",
		"SKIP_SHUTDOWN_CLEANUP": map[bool]string{true: "true", false: "false"}[skipCleanup],
	} {
		t.Setenv(name, value)
	}
	// A previous run's file must be replaced with fresh startup health.
	if err := os.WriteFile(healthFile, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &startupLogs{changed: make(chan struct{}, 1)}
	previousLogger := log.Logger
	log.Logger = zerolog.New(logs)
	t.Cleanup(func() { log.Logger = previousLogger })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); runAgent(ctx) }()
	t.Cleanup(func() {
		cancel()
		// Always join before restoring the global logger, including when an
		// assertion fails while the agent is still running.
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			panic("agent did not stop after cancellation")
		}
	})
	return logs, socket, healthFile, cancel, done
}

type startupLogs struct {
	mu       sync.Mutex
	messages string
	changed  chan struct{}
}

func (l *startupLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.messages += string(p)
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (l *startupLogs) contains(message string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.messages, message)
}

func (l *startupLogs) wait(t *testing.T, message string) {
	t.Helper()
	for !l.contains(message) {
		receiveStartup(t, l.changed)
	}
}

func receiveStartup[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for startup/shutdown")
		var zero T
		return zero
	}
}

func waitStartupHealth(t *testing.T, path string) health.Status {
	t.Helper()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status health.Status
		data, err := os.ReadFile(path) //nolint:gosec // G304: the test's health file.
		if err == nil && json.Unmarshal(data, &status) == nil && status.PID != 0 {
			return status
		}
		select {
		case <-deadline:
			t.Fatal("startup health was not written")
		case <-ticker.C:
		}
	}
}
