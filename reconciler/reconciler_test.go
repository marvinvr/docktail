package reconciler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/marvinvr/docktail/docker"
)

func TestRunAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// No clients: a canceled run must not reconcile or open an event stream.
	r := NewReconciler(nil, nil, time.Hour)
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func TestRunCancelsDockerEventRetry(t *testing.T) {
	for _, scenario := range []string{"initial reconcile", "active stream", "retry delay"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var streams atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/v1.47/containers/json":
					// Keep the initial reconcile independent of the Tailscale CLI.
					http.Error(w, "Docker unavailable", http.StatusServiceUnavailable)
				case "/v1.47/events":
					streams.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					if scenario == "active stream" {
						cancel()
						<-req.Context().Done()
					}
					// Otherwise EOF triggers a retry. The log below cancels only
					// after the reconciler has entered that error path.
				default:
					http.NotFound(w, req)
				}
			}))
			defer srv.Close()
			t.Setenv("DOCKER_HOST", srv.URL)
			t.Setenv("DOCKER_API_VERSION", "1.47")
			t.Setenv("DOCKER_TLS_VERIFY", "")
			t.Setenv("DOCKER_CERT_PATH", "")
			dc, err := docker.NewClient(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dc.Close() }()
			r := NewReconciler(dc, nil, time.Hour)
			if scenario == "initial reconcile" {
				r.SetResultHook(func(error) { cancel() })
			}
			previousLogger := log.Logger
			log.Logger = zerolog.New(cancelRetryLog{cancel: cancel, enabled: scenario == "retry delay"})
			defer func() { log.Logger = previousLogger }()
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("Run = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				cancel()
				t.Error("shutdown waited for the Docker event retry delay")
				// Join before restoring the global logger even if the regression
				// returns; the old retry delay must not cause a teardown race.
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("Run after late cancel = %v, want context.Canceled", err)
					}
				case <-time.After(30 * time.Second):
					panic("reconciler did not stop after cancellation")
				}
			}
			wantStreams := int32(1)
			if scenario == "initial reconcile" {
				wantStreams = 0
			}
			if got := streams.Load(); got != wantStreams {
				t.Errorf("opened %d event streams, want %d", got, wantStreams)
			}
		})
	}
}

type cancelRetryLog struct {
	cancel  context.CancelFunc
	enabled bool
}

func (l cancelRetryLog) Write(p []byte) (int, error) {
	if l.enabled && bytes.Contains(p, []byte("Docker event stream error")) {
		l.cancel()
	}
	return io.Discard.Write(p)
}
