package cloud

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/marvinvr/docktail/cloud/proto"
	"github.com/marvinvr/docktail/docker"
	apptypes "github.com/marvinvr/docktail/types"
)

// cloudLabels is what a container's docktail.cloud.* labels say, after
// validation. Invalid values are dropped (never guessed at) and described in
// problems so the collector can warn about them.
type cloudLabels struct {
	ignored  bool               // docktail.cloud.ignore=true
	intent   *proto.LabelIntent // nil when no valid intent label is set
	problems []string           // one line per dropped or unknown label, in label order
}

// parseCloudLabels validates a container's docktail.cloud.* labels. The same
// bounds apply as to cloud-pushed config (proto.SanitizeLabelIntent), so a
// label can never shape a check the cloud itself could not have sent.
func parseCloudLabels(labels map[string]string) cloudLabels {
	var out cloudLabels
	if len(labels) == 0 {
		return out
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var raw proto.LabelIntent
	for _, key := range keys {
		value := strings.TrimSpace(labels[key])
		switch key {
		case apptypes.LabelCloudIgnore:
			switch strings.ToLower(value) {
			case "true":
				out.ignored = true
			case "false":
			default:
				out.problems = append(out.problems, fmt.Sprintf("%s=%q ignored: must be true or false", key, value))
			}
		case apptypes.LabelCloudLogs:
			if strings.EqualFold(value, proto.LogModeOff) {
				raw.Logs = proto.LogModeOff
			} else {
				out.problems = append(out.problems, fmt.Sprintf("%s=%q ignored: the only value is off (capture itself is switched on in DockTail Cloud)", key, value))
			}
		case apptypes.LabelCloudCheckKind:
			switch kind := strings.ToLower(value); kind {
			case "tcp", "http":
				raw.CheckKind = kind
			default:
				out.problems = append(out.problems, fmt.Sprintf("%s=%q ignored: must be tcp or http", key, value))
			}
		case apptypes.LabelCloudCheckPath:
			if value == "" || proto.ValidateHTTPPath(value) != nil {
				out.problems = append(out.problems, fmt.Sprintf("%s=%q ignored: must be a relative path starting with /", key, value))
			} else {
				raw.CheckPath = value
			}
		case apptypes.LabelCloudCheckExpectStatus:
			code, err := strconv.Atoi(value)
			if err != nil || code < 100 || code > 599 {
				out.problems = append(out.problems, fmt.Sprintf("%s=%q ignored: must be an HTTP status code (100-599)", key, value))
			} else {
				raw.CheckExpectStatus = code
			}
		default:
			out.problems = append(out.problems, fmt.Sprintf("%s: unknown DockTail Cloud label, ignored", key))
		}
	}
	if raw.CheckKind == "tcp" && (raw.CheckPath != "" || raw.CheckExpectStatus != 0) {
		out.problems = append(out.problems, fmt.Sprintf("%s and %s ignored: %s=tcp has no HTTP request",
			apptypes.LabelCloudCheckPath, apptypes.LabelCloudCheckExpectStatus, apptypes.LabelCloudCheckKind))
	}
	out.intent, _ = proto.SanitizeLabelIntent(&raw)
	return out
}

// applyLabelIntent lays a service's label intent over the cloud's check config
// for it (base; nil when the cloud sent none). Each label wins for its own
// field, a field with no label keeps the cloud's value, and a path or expected
// status with no kind label implies http. ok is false when the labels say
// nothing about the check (or the result would not validate): keep base.
func applyLabelIntent(serviceKey string, base *proto.CheckConfig, intent *proto.LabelIntent) (proto.CheckConfig, bool) {
	if intent == nil || (intent.CheckKind == "" && intent.CheckPath == "" && intent.CheckExpectStatus == 0) {
		return proto.CheckConfig{}, false
	}
	cfg := proto.CheckConfig{Kind: "tcp", IntervalMS: proto.DefaultCheckIntervalMS}
	if base != nil {
		cfg = *base
	}
	cfg.ServiceKey = serviceKey
	if cfg.IntervalMS == 0 {
		cfg.IntervalMS = proto.DefaultCheckIntervalMS
	}
	switch {
	case intent.CheckKind != "":
		cfg.Kind = intent.CheckKind
	case intent.CheckPath != "" || intent.CheckExpectStatus != 0:
		cfg.Kind = "http"
	}
	if intent.CheckPath != "" {
		cfg.Path = intent.CheckPath
	}
	if intent.CheckExpectStatus != 0 {
		cfg.ExpectStatus = intent.CheckExpectStatus
	}
	if cfg.Kind == "tcp" {
		cfg.Path = ""
		cfg.ExpectStatus = 0
	}
	if proto.ValidateCheckConfig(cfg) != nil {
		return proto.CheckConfig{}, false
	}
	return cfg, true
}

// intentForService narrows a container's label intent to one of the services
// it publishes. Cloud labels are container-wide, but a service whose backend
// speaks TCP (docktail.service[.N].protocol=tcp or tls-terminated-tcp) cannot
// answer a plain HTTP check, so for it an HTTP-shaping label (kind http, a path, an expected
// status) is dropped and it keeps its TCP check; logs=off still applies.
func intentForService(intent *proto.LabelIntent, backendProtocol string) *proto.LabelIntent {
	if p := strings.ToLower(backendProtocol); intent == nil || (p != "tcp" && p != "tls-terminated-tcp") {
		return intent
	}
	out := *intent
	if out.CheckKind == "http" {
		out.CheckKind = ""
	}
	out.CheckPath = ""
	out.CheckExpectStatus = 0
	if out == (proto.LabelIntent{}) {
		return nil
	}
	return &out
}

// labelsForbidCapture reports whether a container's labels (a docker event's
// actor attributes carry them all) rule out incident log capture: the
// container is ignored by DockTail Cloud, or has docktail.cloud.logs=off.
func labelsForbidCapture(labels map[string]string) bool {
	if docker.IsCloudIgnored(labels) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(labels[apptypes.LabelCloudLogs]), proto.LogModeOff)
}
