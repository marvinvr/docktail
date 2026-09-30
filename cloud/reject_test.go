package cloud

import (
	"strings"
	"testing"
	"time"

	"github.com/marvinvr/docktail/cloud/proto"
)

func TestHelloRejectionActions(t *testing.T) {
	cases := []struct {
		code   proto.RejectCode
		action rejectAction
		want   string // a fragment the operator hint must carry
	}{
		{proto.RejectInvalidKey, rejectStop, "/settings/agent-keys"},
		{proto.RejectBlocked, rejectRetryRare, "/hosts"},
		{proto.RejectProtocolMismatch, rejectRetryRare, "protocol v"},
		{proto.RejectEnrollmentClosed, rejectRetrySlow, "/settings/agent-keys"},
		{proto.RejectOverCap, rejectRetrySlow, "/settings/billing"},
		{proto.RejectDuplicate, rejectRetry, "retries automatically"},
		{"", rejectRetry, "retries automatically"},
		{"some_future_code", rejectRetry, "some_future_code"},
	}
	for _, tc := range cases {
		r := helloRejection(tc.code)
		if r.action != tc.action {
			t.Errorf("%q: action = %d, want %d", tc.code, r.action, tc.action)
		}
		if r.reason != string(tc.code) {
			t.Errorf("%q: reason = %q", tc.code, r.reason)
		}
		if !strings.Contains(r.hint, tc.want) {
			t.Errorf("%q: hint %q does not mention %q", tc.code, r.hint, tc.want)
		}
		if r.action == rejectRetryRare {
			if g := r.gaveUp(); g.action != rejectStop || g.reason != r.reason || !strings.Contains(g.hint, tc.want) {
				t.Errorf("%q: gaveUp() = %+v", tc.code, g)
			}
		}
	}
}

func TestHTTPRejection(t *testing.T) {
	if r := httpRejection(401); r == nil || r.action != rejectStop || !strings.Contains(r.hint, EnvKey) {
		t.Errorf("401: got %+v, want a terminal key rejection", r)
	}
	if r := httpRejection(403); r == nil || r.action != rejectRetryRare || strings.Contains(r.hint, EnvKey) || r.stopHint == "" {
		t.Errorf("403: got %+v, want a rare retry that does not blame the key", r)
	}
	for _, status := range []int{400, 429, 500, 502, 503} {
		if r := httpRejection(status); r != nil {
			t.Errorf("status %d: got %+v, want a retryable dial failure", status, r)
		}
	}
}

func TestBackoffAround(t *testing.T) {
	b := newBackoff()
	for i := 0; i < 100; i++ {
		d := b.around(rareRetryInterval)
		if d < 12*time.Minute || d > 18*time.Minute {
			t.Fatalf("around(%s) = %s, want within ±20%%", rareRetryInterval, d)
		}
	}
}
