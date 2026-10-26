package docker

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

func TestMemUsageWithoutFileCache(t *testing.T) {
	tests := []struct {
		name  string
		usage uint64
		stats map[string]uint64
		want  int64
	}{
		{"v2 excludes active and inactive cache", 1000, map[string]uint64{"file": 800, "inactive_file": 100}, 200},
		{"v1 excludes active and inactive cache", 1000, map[string]uint64{"total_cache": 800, "total_inactive_file": 100}, 200},
		{"v2 working set fallback", 1000, map[string]uint64{"inactive_file": 100}, 900},
		{"v1 working set fallback", 1000, map[string]uint64{"total_inactive_file": 100}, 900},
		{"raw usage fallback", 1000, nil, 1000},
		{"zero cache is present", 1000, map[string]uint64{"file": 0, "inactive_file": 100}, 1000},
		{"all cache v2", 1000, map[string]uint64{"file": 1000}, 0},
		{"all cache v1", 1000, map[string]uint64{"total_cache": 1000}, 0},
		{"cache exceeds usage v2", 1000, map[string]uint64{"file": 1001}, 0},
		{"cache exceeds usage v1", 1000, map[string]uint64{"total_cache": 1001}, 0},
		{"zero usage", 0, map[string]uint64{"file": 0}, 0},
		{"saturates after subtracting cache", math.MaxUint64, map[string]uint64{"file": 1}, math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := memUsageWithoutFileCache(container.MemoryStats{Usage: tt.usage, Stats: tt.stats})
			if got != tt.want {
				t.Fatalf("used = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRunningContainersMemoryExcludesFileCache(t *testing.T) {
	// Both cgroup versions retain active cache in individual container metrics,
	// while the CT host total excludes it. Older stats retain the working set.
	stats := map[string]container.MemoryStats{
		"v2":       {Usage: 1000, Stats: map[string]uint64{"file": 800, "inactive_file": 100}},
		"v1":       {Usage: 2000, Stats: map[string]uint64{"total_cache": 1500, "total_inactive_file": 200}},
		"fallback": {Usage: 300, Stats: map[string]uint64{"inactive_file": 50}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/v1.47")
		if path == "/containers/json" {
			_ = json.NewEncoder(w).Encode([]container.Summary{{ID: "v2"}, {ID: "v1"}, {ID: "fallback"}})
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/stats")
		if mem, ok := stats[id]; ok {
			_ = json.NewEncoder(w).Encode(container.StatsResponse{MemoryStats: mem})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost(srv.URL), client.WithVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{cli: cli}
	t.Cleanup(func() { _ = c.Close() })
	ctx := context.Background()

	for id, want := range map[string]int64{"v2": 900, "v1": 1800, "fallback": 250} {
		s, err := c.ContainerStats(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if s.MemUsageBytes != want {
			t.Errorf("%s container working set = %d, want %d", id, s.MemUsageBytes, want)
		}
	}
	used, err := c.RunningContainersMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(200 + 500 + 250); used != want {
		t.Fatalf("CT used = %d, want %d", used, want)
	}
}
