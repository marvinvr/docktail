package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marvinvr/docktail/cloud/proto"
)

// cloudDegradedLatencyMS mirrors DockTail Cloud's degraded threshold: a
// successful local probe slower than this is reported as degraded (a warning)
// instead of down. The agent does not read it; it only has to keep its probe
// timeouts above it so a slow-but-healthy service can succeed slowly.
const cloudDegradedLatencyMS = 5000

func serverTarget(srv *httptest.Server) string {
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestProbeTimeoutsExceedDegradedThreshold(t *testing.T) {
	for name, timeout := range map[string]time.Duration{"dial": dialTimeout, "http": httpTimeout} {
		if timeout.Milliseconds() <= cloudDegradedLatencyMS {
			t.Errorf("%s timeout %v must exceed the %dms degraded threshold, or slow probes fail instead of degrading",
				name, timeout, cloudDegradedLatencyMS)
		}
	}
}

// A slow-but-answering HTTP endpoint must come back OK with a latency above the
// degraded threshold (→ degraded), not as a failed, timed-out probe (→ down).
func TestHTTPCheckSlowResponseSucceedsSlowly(t *testing.T) {
	if testing.Short() {
		t.Skip("waits past the degraded threshold")
	}
	delay := time.Duration(cloudDegradedLatencyMS)*time.Millisecond + 500*time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := newChecker().httpCheck(context.Background(), "svc", serverTarget(srv), "/", 0)
	if !res.OK {
		t.Fatalf("slow response reported as failure: class=%q error=%q", res.Class, res.Error)
	}
	if res.LatencyMS <= cloudDegradedLatencyMS {
		t.Fatalf("latency = %dms, want > %dms so the cloud classifies it degraded", res.LatencyMS, cloudDegradedLatencyMS)
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		code      int
		expect    int
		wantOK    bool
		wantClass string
	}{
		{"2xx without expectation", 200, 0, true, ""},
		{"4xx without expectation is reachable", 404, 0, true, ""},
		{"3xx without expectation is reachable", 302, 0, true, ""},
		{"5xx without expectation", 503, 0, false, proto.ClassHTTP5xx},
		{"matches expectation", 204, 204, true, ""},
		{"expected 5xx is healthy", 503, 503, true, ""},
		{"non-5xx mismatch", 404, 200, false, proto.ClassHTTPStatus},
		{"unfollowed redirect mismatch", 301, 200, false, proto.ClassHTTPStatus},
		{"5xx mismatch stays 5xx", 500, 200, false, proto.ClassHTTP5xx},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, class := classifyHTTPStatus(tc.code, tc.expect)
			if ok != tc.wantOK || class != tc.wantClass {
				t.Fatalf("classifyHTTPStatus(%d, %d) = (%v, %q), want (%v, %q)",
					tc.code, tc.expect, ok, class, tc.wantOK, tc.wantClass)
			}
		})
	}
}

func TestHTTPCheckExpectedStatusMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	res := newChecker().httpCheck(context.Background(), "svc", serverTarget(srv), "/healthz", http.StatusOK)
	if res.OK || res.Class != proto.ClassHTTPStatus || res.StatusCode != http.StatusNotFound {
		t.Fatalf("got ok=%v class=%q status=%d, want a failed %q probe with status 404",
			res.OK, res.Class, res.StatusCode, proto.ClassHTTPStatus)
	}
}

func TestHTTPCheckInvalidPathIsUnclassified(t *testing.T) {
	res := newChecker().httpCheck(context.Background(), "svc", "127.0.0.1:1", "not-a-path", 0)
	if res.OK || res.Class != "" {
		t.Fatalf("got ok=%v class=%q, want an unclassified failure", res.OK, res.Class)
	}
}
