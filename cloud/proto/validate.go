package proto

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateCheckConfig validates the bounded, non-routing configuration the
// cloud may send for one locally discovered service.
func ValidateCheckConfig(cfg CheckConfig) error {
	if !ValidServiceKey(cfg.ServiceKey) {
		return fmt.Errorf("invalid service key")
	}
	if cfg.Kind != "tcp" && cfg.Kind != "http" {
		return fmt.Errorf("invalid check kind")
	}
	if cfg.IntervalMS < MinCheckIntervalMS || cfg.IntervalMS > MaxCheckIntervalMS {
		return fmt.Errorf("check interval outside allowed range")
	}
	if cfg.ExpectStatus != 0 && (cfg.ExpectStatus < 100 || cfg.ExpectStatus > 599) {
		return fmt.Errorf("invalid expected HTTP status")
	}
	if cfg.Kind == "http" {
		if err := ValidateHTTPPath(cfg.Path); err != nil {
			return err
		}
	}
	return nil
}

// ValidServiceKey reports whether a cloud-config key is safe and bounded. It
// remains an opaque identifier; validation never rewrites it.
func ValidServiceKey(key string) bool {
	return key != "" && len(key) <= MaxCheckServiceKeyBytes &&
		utf8.ValidString(key) && !containsControl(key)
}

// SanitizeCheckConfigs returns at most MaxCheckConfigs valid entries. Invalid
// and over-limit entries are omitted so a bad stored config cannot make the
// whole agent configuration unusable.
func SanitizeCheckConfigs(checks []CheckConfig) (valid []CheckConfig, rejected int) {
	capacity := len(checks)
	if capacity > MaxCheckConfigs {
		capacity = MaxCheckConfigs
	}
	valid = make([]CheckConfig, 0, capacity)
	for _, cfg := range checks {
		// Older protocol-v1 senders omitted interval_ms. Preserve their check
		// shape with the current default while still rejecting out-of-range
		// explicit values.
		if cfg.IntervalMS == 0 {
			cfg.IntervalMS = DefaultCheckIntervalMS
		}
		if len(valid) >= MaxCheckConfigs || ValidateCheckConfig(cfg) != nil {
			rejected++
			continue
		}
		valid = append(valid, cfg)
	}
	return valid, rejected
}

// SanitizeLogOverrides bounds and validates per-service log settings.
func SanitizeLogOverrides(overrides map[string]string) (valid map[string]string, rejected int) {
	capacity := len(overrides)
	if capacity > MaxLogOverrides {
		capacity = MaxLogOverrides
	}
	valid = make(map[string]string, capacity)
	for key, mode := range overrides {
		if len(valid) >= MaxLogOverrides || !ValidServiceKey(key) ||
			(mode != LogModeIncident && mode != LogModeOff) {
			rejected++
			continue
		}
		valid[key] = mode
	}
	return valid, rejected
}

// SafeLogMode fails closed for missing, invalid, and reserved modes.
func SafeLogMode(mode string) string {
	if mode == LogModeIncident {
		return LogModeIncident
	}
	return LogModeOff
}

// ValidateHTTPPath bounds a relative HTTP request path. It is the one rule for
// every path the cloud puts on the wire or dials itself: the check config it
// sends an agent, and the Funnel path the public vantage requests. An empty path
// is valid and means "/".
func ValidateHTTPPath(path string) error {
	if path == "" {
		return nil
	}
	if len(path) > MaxCheckPathBytes || !utf8.ValidString(path) ||
		containsControl(path) || strings.Contains(path, "\\") {
		return fmt.Errorf("invalid HTTP path")
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.Contains(path, "#") {
		return fmt.Errorf("HTTP path must be relative")
	}
	parsed, err := url.ParseRequestURI(path)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Fragment != "" ||
		strings.HasPrefix(parsed.Path, "//") || strings.Contains(parsed.Path, "\\") ||
		containsControl(parsed.Path) {
		return fmt.Errorf("invalid relative HTTP path")
	}
	return nil
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
