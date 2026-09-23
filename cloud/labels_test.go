package cloud

import (
	"testing"

	"github.com/marvinvr/docktail/cloud/proto"
)

func TestParseCloudLabels(t *testing.T) {
	tests := []struct {
		name         string
		labels       map[string]string
		wantIgnored  bool
		wantIntent   *proto.LabelIntent
		wantProblems int
	}{
		{name: "none", labels: nil},
		{
			name:        "ignore",
			labels:      map[string]string{"docktail.cloud.ignore": "True"},
			wantIgnored: true,
		},
		{
			name:   "ignore false",
			labels: map[string]string{"docktail.cloud.ignore": "false"},
		},
		{
			name: "full intent",
			labels: map[string]string{
				"docktail.cloud.logs":                "off",
				"docktail.cloud.check.kind":          "HTTP",
				"docktail.cloud.check.path":          "/healthz?deep=1",
				"docktail.cloud.check.expect-status": "204",
			},
			wantIntent: &proto.LabelIntent{Logs: "off", CheckKind: "http", CheckPath: "/healthz?deep=1", CheckExpectStatus: 204},
		},
		{
			name: "invalid values are dropped, valid ones kept",
			labels: map[string]string{
				"docktail.cloud.logs":                "incident",
				"docktail.cloud.check.kind":          "udp",
				"docktail.cloud.check.path":          "//evil.example/x",
				"docktail.cloud.check.expect-status": "700",
				"docktail.cloud.ignore":              "yes",
				"docktail.cloud.check.interval":      "10s",
			},
			wantProblems: 6,
		},
		{
			name: "tcp drops http-only fields",
			labels: map[string]string{
				"docktail.cloud.check.kind": "tcp",
				"docktail.cloud.check.path": "/healthz",
			},
			wantIntent:   &proto.LabelIntent{CheckKind: "tcp"},
			wantProblems: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCloudLabels(tt.labels)
			if got.ignored != tt.wantIgnored {
				t.Errorf("ignored = %v, want %v", got.ignored, tt.wantIgnored)
			}
			if (got.intent == nil) != (tt.wantIntent == nil) ||
				(got.intent != nil && *got.intent != *tt.wantIntent) {
				t.Errorf("intent = %+v, want %+v", got.intent, tt.wantIntent)
			}
			if len(got.problems) != tt.wantProblems {
				t.Errorf("problems = %q, want %d", got.problems, tt.wantProblems)
			}
		})
	}
}

func TestApplyLabelIntent(t *testing.T) {
	cloudHTTP := &proto.CheckConfig{ServiceKey: "web:443", Kind: "http", Path: "/", ExpectStatus: 200, IntervalMS: 60_000}

	if _, ok := applyLabelIntent("web:443", cloudHTTP, nil); ok {
		t.Fatal("no intent must keep the cloud config")
	}
	if _, ok := applyLabelIntent("web:443", cloudHTTP, &proto.LabelIntent{Logs: "off"}); ok {
		t.Fatal("a logs-only intent says nothing about the check")
	}

	// A path alone implies http over the TCP default.
	got, ok := applyLabelIntent("web:443", nil, &proto.LabelIntent{CheckPath: "/healthz"})
	if !ok || got.Kind != "http" || got.Path != "/healthz" || got.IntervalMS != proto.DefaultCheckIntervalMS {
		t.Fatalf("path only: got %+v ok=%v", got, ok)
	}

	// Label wins field by field; unlabelled fields keep the cloud's values.
	got, ok = applyLabelIntent("web:443", cloudHTTP, &proto.LabelIntent{CheckPath: "/ready"})
	if !ok || got.Kind != "http" || got.Path != "/ready" || got.ExpectStatus != 200 || got.IntervalMS != 60_000 {
		t.Fatalf("merge: got %+v ok=%v", got, ok)
	}

	// An explicit tcp label turns a cloud HTTP check back into TCP.
	got, ok = applyLabelIntent("web:443", cloudHTTP, &proto.LabelIntent{CheckKind: "tcp"})
	if !ok || got.Kind != "tcp" || got.Path != "" || got.ExpectStatus != 0 {
		t.Fatalf("tcp: got %+v ok=%v", got, ok)
	}
}

func TestLabelsForbidCapture(t *testing.T) {
	if labelsForbidCapture(map[string]string{"name": "web"}) {
		t.Fatal("no labels must not forbid capture")
	}
	if !labelsForbidCapture(map[string]string{"docktail.cloud.logs": "off"}) {
		t.Fatal("logs=off must forbid capture")
	}
	if !labelsForbidCapture(map[string]string{"docktail.cloud.ignore": "true"}) {
		t.Fatal("ignore=true must forbid capture")
	}
}

func TestIntentForService(t *testing.T) {
	intent := &proto.LabelIntent{Logs: "off", CheckKind: "http", CheckPath: "/healthz"}
	if got := intentForService(intent, "http"); got != intent {
		t.Fatalf("http backend must keep the intent, got %+v", got)
	}
	got := intentForService(intent, "tcp")
	if got == nil || *got != (proto.LabelIntent{Logs: "off"}) {
		t.Fatalf("tcp backend: got %+v, want logs only", got)
	}
	if got := intentForService(&proto.LabelIntent{CheckPath: "/healthz"}, "tls-terminated-tcp"); got != nil {
		t.Fatalf("tcp backend with only HTTP intent: got %+v, want nil", got)
	}
}
