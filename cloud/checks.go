package cloud

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/marvinvr/docktail/cloud/proto"
)

const (
	// dialTimeout bounds the local TCP probe's connect. Kept well above DockTail
	// Cloud's 5s "degraded" latency threshold so a busy-but-healthy service that
	// takes a few seconds to accept a connection is reported slow (degraded),
	// not timed out and misclassified as a critical container-down.
	dialTimeout = 15 * time.Second
	// httpTimeout bounds the whole local HTTP exchange (connect through response
	// headers). It must also stay well above the 5s degraded threshold: a
	// timeout at or below it would turn every slow-but-answering endpoint into a
	// failed probe, making "degraded" unreachable for HTTP checks.
	httpTimeout = dialTimeout
)

// checker runs local-vantage probes. The checker only produces the "local"
// vantage; the collector contributes "tailnet" from the host's `tailscale serve`
// config (see tailnet.go) and a public probe would contribute "public".
type checker struct {
	httpClient *http.Client
}

func newChecker() *checker {
	return &checker{
		httpClient: &http.Client{
			Timeout: httpTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type serviceCheck struct {
	service proto.Service
	cfg     *proto.CheckConfig
}

// run probes every service once. Cloud check config (keyed by service key)
// selects the check shape but never its destination; services with no locally
// discovered target are skipped.
func (c *checker) run(ctx context.Context, services []proto.Service, configs []proto.CheckConfig) []proto.CheckResult {
	configs, _ = proto.SanitizeCheckConfigs(configs)
	cfgByKey := make(map[string]proto.CheckConfig, len(configs))
	for _, cc := range configs {
		cfgByKey[cc.ServiceKey] = cc
	}

	results := make([]proto.CheckResult, 0, len(services))
	for _, svc := range services {
		sc := serviceCheck{service: svc}
		if cc, ok := cfgByKey[svc.Key]; ok {
			cfg := cc
			sc.cfg = &cfg
		}
		if res, ok := c.runOne(ctx, sc); ok {
			results = append(results, res)
		}
	}
	return results
}

func (c *checker) runOne(ctx context.Context, sc serviceCheck) (proto.CheckResult, bool) {
	svc := sc.service
	if resolveKind(sc) == "http" {
		target, path, expect, ok := resolveHTTP(sc)
		if !ok {
			return proto.CheckResult{}, false
		}
		return c.httpCheck(ctx, svc.Key, target, path, expect), true
	}
	target, ok := resolveTCP(sc)
	if !ok {
		return proto.CheckResult{}, false
	}
	return c.tcpCheck(ctx, svc.Key, target), true
}

func resolveKind(sc serviceCheck) string {
	if sc.cfg != nil && sc.cfg.Kind != "" {
		return strings.ToLower(sc.cfg.Kind)
	}
	return "tcp"
}

func resolveTCP(sc serviceCheck) (string, bool) {
	svc := sc.service
	host, port := checkHostPort(svc)
	if host == "" || port == "" {
		return "", false
	}
	return net.JoinHostPort(host, port), true
}

// checkHostPort picks the address the LOCAL check should dial: the locally
// discovered CheckIP/CheckPort attached when the serve destination isn't reachable
// from inside the agent's container (published-port mode → container IP; host-network
// mode → docker host gateway) when present, else the serve destination
// IPAddress/TargetPort (direct mode, or the agent sharing the host netns).
func checkHostPort(svc proto.Service) (host, port string) {
	host = firstNonEmpty(svc.CheckIP, svc.IPAddress)
	port = firstNonEmpty(svc.CheckPort, svc.TargetPort, svc.Port)
	return host, port
}

func resolveHTTP(sc serviceCheck) (target, path string, expect int, ok bool) {
	svc := sc.service
	path = "/"
	if sc.cfg != nil {
		if sc.cfg.Path != "" {
			path = sc.cfg.Path
		}
		expect = sc.cfg.ExpectStatus
	}
	host, port := checkHostPort(svc)
	if host == "" || port == "" {
		return "", "", 0, false
	}
	target = net.JoinHostPort(host, port)
	return target, path, expect, true
}

func (c *checker) tcpCheck(ctx context.Context, key, target string) proto.CheckResult {
	res := proto.CheckResult{ServiceKey: key, Vantage: proto.VantageLocal, Kind: "tcp", CheckedAt: nowMillis()}
	start := time.Now()
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", target)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		res.Class = classifyDialError(err)
		return res
	}
	_ = conn.Close()
	res.OK = true
	return res
}

func (c *checker) httpCheck(ctx context.Context, key, target, path string, expect int) proto.CheckResult {
	res := proto.CheckResult{ServiceKey: key, Vantage: proto.VantageLocal, Kind: "http", CheckedAt: nowMillis()}
	start := time.Now()
	// A bad path is a configuration problem, not a transport fault, so it is
	// left unclassified rather than reported as a refused connection. It is not
	// expected in practice: SanitizeCheckConfigs already drops configs whose
	// path fails the same parse, so such a service falls back to a TCP check.
	relative, err := url.ParseRequestURI(path)
	if err != nil {
		res.LatencyMS = time.Since(start).Milliseconds()
		res.OK = false
		res.Error = "invalid relative HTTP check path"
		return res
	}
	checkURL := (&url.URL{
		Scheme:   "http",
		Host:     target,
		Path:     relative.Path,
		RawPath:  relative.RawPath,
		RawQuery: relative.RawQuery,
	}).String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
	if err != nil {
		// Building the request never touched the network: unclassified, as above.
		res.LatencyMS = time.Since(start).Milliseconds()
		res.OK = false
		res.Error = err.Error()
		return res
	}
	resp, err := c.httpClient.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		res.OK = false
		res.Error = err.Error()
		res.Class = classifyDialError(err)
		return res
	}
	defer func() { _ = resp.Body.Close() }()

	res.StatusCode = resp.StatusCode
	res.OK, res.Class = classifyHTTPStatus(resp.StatusCode, expect)
	return res
}

// classifyHTTPStatus judges a response the endpoint actually returned. A
// configured expected status is authoritative: only that exact code passes, so
// an endpoint deliberately expected to answer 503 is healthy when it does. A
// miss is http_5xx when the server errored and http_status otherwise (e.g. a
// 404 or an unfollowed redirect where 200 was expected). Without an expected
// status, any answer below 500 proves the endpoint is reachable.
func classifyHTTPStatus(code, expect int) (ok bool, class string) {
	switch {
	case expect > 0 && code == expect:
		return true, ""
	case code >= 500:
		return false, proto.ClassHTTP5xx
	case expect > 0:
		return false, proto.ClassHTTPStatus
	default:
		return true, ""
	}
}

func classifyDialError(err error) string {
	if err == nil {
		return ""
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return proto.ClassTimeout
	}
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return proto.ClassTLS
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return proto.ClassDNS
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return proto.ClassRefused
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return proto.ClassDNS
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return proto.ClassTimeout
	case strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return proto.ClassTLS
	default:
		return proto.ClassContainer
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func nowMillis() int64 { return time.Now().UnixMilli() }
