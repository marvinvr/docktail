package proto

import (
	"encoding/json"
	"fmt"
)

// MessageType is the discriminator on an [Envelope].
type MessageType string

// Agent -> Cloud message types.
const (
	TypeHello        MessageType = "hello"         // first frame after connect
	TypeSnapshot     MessageType = "snapshot"      // service catalog snapshot
	TypeContainers   MessageType = "containers"    // non-docktail container inventory (metadata only)
	TypeEvent        MessageType = "event"         // a single docker failure signal
	TypeCheckResults MessageType = "check_results" // batched local-vantage probes
	TypeLogExcerpt   MessageType = "log_excerpt"   // opt-in last N lines on incident
	TypeHeartbeat    MessageType = "heartbeat"     // liveness, every HeartbeatInterval
	TypeHostMetrics  MessageType = "host_metrics"  // periodic whole-host resource vitals
	TypeTailnet      MessageType = "tailnet"       // local netmap view: peer device liveness
)

// Cloud -> Agent message types.
const (
	TypeHelloAck MessageType = "hello_ack" // accept/reject + assigned host id
	TypeConfig   MessageType = "config"    // check config + log opt-in flags
)

// Envelope wraps every frame on the wire. Payload is the JSON of the concrete
// message identified by Type.
type Envelope struct {
	Type    MessageType     `json:"type"`
	TS      int64           `json:"ts"` // unix milliseconds the frame was produced
	Payload json.RawMessage `json:"payload"`
}

// Encode marshals msg into an Envelope of the given type at time tsMillis.
func Encode(t MessageType, tsMillis int64, msg any) ([]byte, error) {
	p, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("proto: marshal payload %s: %w", t, err)
	}
	return json.Marshal(Envelope{Type: t, TS: tsMillis, Payload: p})
}

// Decode unmarshals dst from an Envelope's payload.
func (e Envelope) Decode(dst any) error {
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("proto: unmarshal %s payload: %w", e.Type, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Agent -> Cloud
// ---------------------------------------------------------------------------

// Hello is the first frame. Identity is the Docker engine ID fingerprint; the
// Tailscale node ID is a mutable attribute (can lag at boot) and the hostname
// is display-only.
type Hello struct {
	ProtocolVersion  int      `json:"protocol_version"`
	Fingerprint      string   `json:"fingerprint"`                 // docker engine ID — the host identity & billable unit
	TailscaleNodeID  string   `json:"tailscale_node_id,omitempty"` // mutable attribute, may be empty at boot
	Hostname         string   `json:"hostname,omitempty"`          // display-only, may collide
	AgentVersion     string   `json:"agent_version,omitempty"`
	DockerVersion    string   `json:"docker_version,omitempty"`
	TailscaleVersion string   `json:"tailscale_version,omitempty"`
	Capabilities     []string `json:"capabilities,omitempty"` // e.g. ["http_checks","log_capture"]

	// Static host specs, read once from `docker info` at agent start (display-only).
	OS            string `json:"os,omitempty"`              // docker Info.OperatingSystem, e.g. "Ubuntu 22.04.3 LTS"
	KernelVersion string `json:"kernel_version,omitempty"`  // docker Info.KernelVersion
	Arch          string `json:"arch,omitempty"`            // docker Info.Architecture, e.g. "x86_64"/"aarch64"
	CPUCores      int    `json:"cpu_cores,omitempty"`       // docker Info.NCPU
	MemTotalBytes int64  `json:"mem_total_bytes,omitempty"` // docker Info.MemTotal
}

// Snapshot is the set of monitored services the agent sees. The agent is
// stateless; the cloud diffs snapshots against the catalog to detect
// added/removed/changed services.
//
// Full distinguishes an authoritative snapshot from a partial refresh. A full
// snapshot (the agent's self-discovery, which includes stopped containers) is
// the source of truth for presence: the cloud upserts the listed services and
// treats any catalogued service absent from it as removed. A partial snapshot
// (e.g. the post-reconcile refresh, which only sees running services) upserts
// for enrichment but never drives removals, so a stopped container is not
// mistaken for a deleted one.
type Snapshot struct {
	Services []Service `json:"services"`
	Full     bool      `json:"full"`
}

// Service is one docktail-labeled service as the agent sees it. Mirrors the
// reconciler's view plus runtime status.
type Service struct {
	// Identity (stable within a host).
	Key           string `json:"key"`          // stable key within host (service name + port, else container/funnel key)
	ServiceName   string `json:"service_name"` // tailscale service name (e.g. "svc:myapp")
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	ImageTag      string `json:"image_tag,omitempty"`

	// Compose grouping.
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`

	// Network / exposure.
	FQDN       string `json:"fqdn,omitempty"` // tailnet FQDN (e.g. myapp.tailnet.ts.net)
	IPAddress  string `json:"ip_address,omitempty"`
	Port       string `json:"port,omitempty"`        // tailscale service port
	TargetPort string `json:"target_port,omitempty"` // container/host port behind it
	// CheckIP/CheckPort, when set, are where the agent's LOCAL health check probes,
	// overriding IPAddress/TargetPort for the probe only. Set when the serve target
	// is a host-relative 127.0.0.1 the agent can't reach from inside its own
	// container: published-port services → the container's own IP:port; host-network
	// services → the docker host gateway:containerPort. Empty ⇒ probe IPAddress/
	// TargetPort (direct mode, or the agent on the host netns). Not used by serve.
	CheckIP      string   `json:"check_ip,omitempty"`
	CheckPort    string   `json:"check_port,omitempty"`
	ServiceProto string   `json:"service_protocol,omitempty"` // protocol tailscale serves (https/http/tcp)
	Protocol     string   `json:"protocol,omitempty"`         // protocol the container speaks
	Tags         []string `json:"tags,omitempty"`
	Networks     []string `json:"networks,omitempty"`

	// Funnel (public internet exposure).
	FunnelEnabled  bool   `json:"funnel_enabled"`
	FunnelPort     string `json:"funnel_port,omitempty"`
	FunnelProtocol string `json:"funnel_protocol,omitempty"`
	FunnelPath     string `json:"funnel_path,omitempty"`

	// Runtime status from docker.
	State        string `json:"state"`                   // running/exited/restarting/paused/created
	DockerHealth string `json:"docker_health,omitempty"` // healthy/unhealthy/starting (if container has a healthcheck)
	RestartCount int    `json:"restart_count,omitempty"`

	// Live resource usage from `docker stats` (current value, sampled per
	// snapshot). CPUPercent is a pointer so a genuine 0% (an idle container) is
	// distinct from "not sampled" (nil — e.g. the container isn't running, or the
	// first sample, which has no prior reading to delta against). Memory is zero
	// only when unsampled, since a running container's working set is never 0.
	CPUPercent    *float64 `json:"cpu_percent,omitempty"`     // container CPU usage as % of all host cores
	MemUsageBytes int64    `json:"mem_usage_bytes,omitempty"` // working set (usage minus inactive file cache)
	MemLimitBytes int64    `json:"mem_limit_bytes,omitempty"` // effective limit (container limit, else host total)
}

// Containers is the inventory of NON-docktail containers the agent sees on the
// host — every running/stopped container that is not published as a docktail
// service. Unlike [Snapshot] (the monitored service catalog), these carry only
// descriptive, read-only metadata: there are no checks, vantages, or incidents
// for them, so the cloud never alerts on them. Like Snapshot, a Full message is
// authoritative for presence — the cloud upserts the listed containers and
// treats any catalogued container absent from a full message as gone from
// Docker. This frame is additive and metadata-only; it changes no existing
// message and ProtocolVersion stays 1.
type Containers struct {
	Containers []Container `json:"containers"`
	Full       bool        `json:"full"`
}

// Container is one non-docktail container as the agent sees it: limited,
// read-only metadata only — no exec/deploy surface. Identity is the docker
// ContainerID (stable within a host).
type Container struct {
	ContainerID    string   `json:"container_id"`       // docker container id (short) — identity within host
	IsAgent        bool     `json:"is_agent,omitempty"` // true for the container running this reporting agent
	Name           string   `json:"name"`
	Image          string   `json:"image"`
	ImageTag       string   `json:"image_tag,omitempty"`
	State          string   `json:"state"`            // running/exited/restarting/paused/created
	Status         string   `json:"status,omitempty"` // human status line, e.g. "Up 3 hours (healthy)"
	Health         string   `json:"health,omitempty"` // healthy/unhealthy/starting (best-effort, may be empty)
	ComposeProject string   `json:"compose_project,omitempty"`
	ComposeService string   `json:"compose_service,omitempty"`
	Ports          []string `json:"ports,omitempty"`      // published/exposed port mappings, e.g. "0.0.0.0:8080->80/tcp"
	CreatedAt      int64    `json:"created_at,omitempty"` // unix seconds the container was created

	// Live resource usage from `docker stats` (running containers only),
	// mirroring [Service]. CPUPercent is a pointer so a genuine 0% (an idle
	// container) is distinct from "unknown" (nil — not running, or the first
	// sample with no prior reading to delta against).
	CPUPercent    *float64 `json:"cpu_percent,omitempty"`
	MemUsageBytes int64    `json:"mem_usage_bytes,omitempty"`
	MemLimitBytes int64    `json:"mem_limit_bytes,omitempty"`
}

// Event is a single docker-side failure signal. The kind is the product —
// "why", not just "down".
type Event struct {
	Kind          EventKind `json:"kind"`
	ServiceKey    string    `json:"service_key,omitempty"`
	ContainerID   string    `json:"container_id"`
	ContainerName string    `json:"container_name,omitempty"`
	ExitCode      *int      `json:"exit_code,omitempty"`     // for die
	RestartCount  int       `json:"restart_count,omitempty"` // for restart_loop
	HealthStatus  string    `json:"health_status,omitempty"` // for health_status
	Message       string    `json:"message,omitempty"`
	OccurredAt    int64     `json:"occurred_at"` // unix ms
}

// EventKind enumerates the docker failure signals the agent forwards.
type EventKind string

const (
	EventDie          EventKind = "die"           // container exited (carries exit code)
	EventOOM          EventKind = "oom"           // out-of-memory kill
	EventHealthStatus EventKind = "health_status" // docker healthcheck transition
	EventRestartLoop  EventKind = "restart_loop"  // crash-looping
	EventStart        EventKind = "start"
	EventStop         EventKind = "stop"
)

// CheckResults is a batch of local-vantage probe results.
type CheckResults struct {
	Results []CheckResult `json:"results"`
}

// CheckResult is one probe outcome. Vantage is always "local" from the agent;
// the prober contributes "tailnet" and the public probe contributes "public"
// on the cloud side.
type CheckResult struct {
	ServiceKey string `json:"service_key"`
	Vantage    string `json:"vantage"` // VantageLocal from the agent
	Kind       string `json:"kind"`    // tcp/http
	OK         bool   `json:"ok"`
	LatencyMS  int64  `json:"latency_ms"`
	StatusCode int    `json:"status_code,omitempty"` // http
	Class      string `json:"class,omitempty"`       // failure classification (see Classification constants)
	Error      string `json:"error,omitempty"`
	CheckedAt  int64  `json:"checked_at"` // unix ms
}

// LogExcerpt carries the last N lines of a service's logs on incident. Capture
// is opt-in via LogConfig and capped at MaxLogLines / MaxLogBytes.
type LogExcerpt struct {
	ServiceKey  string   `json:"service_key"`
	ContainerID string   `json:"container_id"`
	Lines       []string `json:"lines"`
	ByteSize    int      `json:"byte_size"`
	CapturedAt  int64    `json:"captured_at"` // unix ms
}

// Heartbeat is emitted every HeartbeatInterval and doubles as liveness.
type Heartbeat struct {
	Uptime int64 `json:"uptime_seconds,omitempty"`
}

// HostMetrics is a periodic sample of whole-host resource utilization, read by
// the agent from the host's own /proc and /sys — the machine, NOT the sum of
// containers (the latter is per-Service CPUPercent/MemUsageBytes). In a normal
// Linux container the system-wide /proc files are the host's, so the agent reads
// true host utilization with no extra mounts; on Docker Desktop they reflect the
// LinuxKit VM, and on platforms without sensors temperature is simply absent.
//
// Every field is optional and best-effort: an agent that cannot read a sensor
// (old agent, no thermal zone, restricted /sys) omits it and the cloud stores it
// as NULL. Like the other agent->cloud frames it is descriptive metadata only —
// no exec surface — and ProtocolVersion stays 1.
type HostMetrics struct {
	// CPUPercent is utilization across all cores, 0..100 (NOT summed-per-core
	// like a container's CPUPercent). A pointer so a genuine 0% (an idle host) is
	// distinct from "not sampled" (nil — the first sample, with no prior
	// /proc/stat reading to delta against).
	CPUPercent *float64 `json:"cpu_percent,omitempty"`

	// Memory, derived from /proc/meminfo. Used is Total-Available (the kernel's
	// own estimate, which discounts reclaimable cache) so it reflects real
	// pressure, not raw "used". Bytes; zero only when unsampled.
	MemTotalBytes  int64 `json:"mem_total_bytes,omitempty"`
	MemUsedBytes   int64 `json:"mem_used_bytes,omitempty"`
	SwapTotalBytes int64 `json:"swap_total_bytes,omitempty"` // 0 ⇒ no swap configured
	SwapUsedBytes  int64 `json:"swap_used_bytes,omitempty"`

	// Load averages from /proc/loadavg (1/5/15 min). Pointers so a genuine 0.0
	// (idle) is distinct from "not sampled".
	Load1  *float64 `json:"load1,omitempty"`
	Load5  *float64 `json:"load5,omitempty"`
	Load15 *float64 `json:"load15,omitempty"`

	// Temperatures from /sys/class/thermal + /sys/class/hwmon, best-effort and
	// often absent (VMs, Docker Desktop). TempMaxC is the hottest reading across
	// all zones — the single number worth a glance; Temps carries the labeled
	// per-zone detail when available.
	TempMaxC *float64      `json:"temp_max_c,omitempty"`
	Temps    []TempReading `json:"temps,omitempty"`
}

// TempReading is one labeled temperature sensor reading, in degrees Celsius.
type TempReading struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

// TailnetReport carries the agent's view of its local tailscale netmap: the
// online/offline status of the tailnet devices (peers) it can see, read from
// `tailscale status` over the local daemon (no API key). It is the signal the
// cloud uses to split a silent host into agent_down (the device is still up on
// the tailnet, only the agent process died) vs host_down (the whole host is
// gone): a peer's report of a now-silent host's device is the only external
// liveness signal available. Additive and metadata-only — no exec surface — and
// ProtocolVersion stays 1. Emitted only when the host is on a tailnet; absent ⇒
// the tailnet vantage degrades to not_configured exactly as before.
type TailnetReport struct {
	Peers []TailnetPeer `json:"peers"`
}

// TailnetPeer is one tailnet device as seen in this host's netmap. NodeID is the
// tailscale StableNodeID, matched against hosts.tailscale_node_id on the cloud
// side. Online is the live reachability the coordination server reports;
// LastSeen (unix ms) is when the peer was last seen, set only when offline.
type TailnetPeer struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname,omitempty"`
	Online   bool   `json:"online"`
	LastSeen int64  `json:"last_seen,omitempty"`
}

// ---------------------------------------------------------------------------
// Cloud -> Agent
// ---------------------------------------------------------------------------

// HelloAck is the cloud's response to [Hello].
type HelloAck struct {
	Accepted      bool       `json:"accepted"`
	HostID        string     `json:"host_id,omitempty"` // server-assigned host UUID
	Reason        RejectCode `json:"reason,omitempty"`  // set when Accepted is false
	ConfigVersion int        `json:"config_version"`    // current config version for this workspace
	ServerTime    int64      `json:"server_time"`       // unix ms, for clock-skew awareness
}

// RejectCode explains a rejected [Hello].
type RejectCode string

const (
	RejectInvalidKey       RejectCode = "invalid_key"
	RejectBlocked          RejectCode = "blocked"            // host row is blocked (per-host revocation)
	RejectDuplicate        RejectCode = "duplicate_identity" // legacy identity-conflict rejection
	RejectEnrollmentClosed RejectCode = "enrollment_closed"  // key is not enrolled for this host and its enrollment window is closed
	RejectProtocolMismatch RejectCode = "protocol_mismatch"
	RejectOverCap          RejectCode = "over_cap" // workspace past host cap grace window
)

// Config pushes per-service check configuration and log-capture settings to the
// agent. There are deliberately no other knobs: no exec, no deploy, no shell.
type Config struct {
	Version int           `json:"version"`
	Checks  []CheckConfig `json:"checks"`
	Logs    LogConfig     `json:"logs"`

	// Unmonitored marks a host the cloud accepts but does not monitor (the
	// workspace is inactive or past its plan host cap). A monitored host omits the field
	// (absent ⇒ monitored, so an old cloud or old agent behaves as before). An
	// agent that sees Unmonitored should throttle to occasional catalog-teaser
	// snapshots plus heartbeats and stop emitting checks/events/log excerpts;
	// the cloud also drops those frames on its side. Checks is empty in an
	// unmonitored config.
	Unmonitored bool `json:"unmonitored,omitempty"`

	// Deprecated: superseded by Logs. Retained for wire compatibility with
	// older agents that only read log_opt_in; new agents prefer Logs when
	// Logs.Mode is set. The cloud no longer populates this field.
	LogOptIn []string `json:"log_opt_in,omitempty"`
}

// LogConfig controls incident log capture. Mode is the workspace default and
// Overrides set a per-service mode; the effective mode for a service is
// Overrides[serviceKey] when present, else Mode (an empty Mode means the default
// LogModeOff). Continuous capture is reserved and not yet emitted.
type LogConfig struct {
	Mode      string            `json:"mode"`                // LogMode*; "" ⇒ LogModeOff
	Overrides map[string]string `json:"overrides,omitempty"` // service_key -> LogMode*
}

// CheckConfig configures a single local-vantage check. The destination is never
// cloud-controlled: the agent derives it from the locally discovered service.
type CheckConfig struct {
	ServiceKey   string `json:"service_key"`
	Kind         string `json:"kind"`           // tcp/http
	Path         string `json:"path,omitempty"` // bounded relative HTTP path; default /
	ExpectStatus int    `json:"expect_status,omitempty"`
	IntervalMS   int64  `json:"interval_ms"`
}

// Cloud-to-agent configuration limits. The frame cap is enforced before JSON
// decoding by the agent; the item and field caps are enforced again after
// decoding and by the control-plane write path.
const (
	MaxCloudFrameBytes      int64 = 256 << 10
	MaxCheckConfigs               = 512
	MaxLogOverrides               = 512
	MaxCheckServiceKeyBytes       = 256
	MaxCheckPathBytes             = 2048
	MinCheckIntervalMS      int64 = 5_000
	DefaultCheckIntervalMS  int64 = 30_000
	MaxCheckIntervalMS      int64 = 300_000
)

// ---------------------------------------------------------------------------
// Shared vocabulary
// ---------------------------------------------------------------------------

// Vantages — the three points a service is observed from.
const (
	VantageLocal   = "local"   // agent -> container IP
	VantageTailnet = "tailnet" // prober -> service FQDN over WireGuard
	VantagePublic  = "public"  // plain HTTP probe for Funnel services
)

// Probe failure classifications. The classification is the product: "why", not
// "down".
const (
	ClassDNS        = "dns"
	ClassTimeout    = "timeout"
	ClassRefused    = "refused"
	ClassTLS        = "tls"
	ClassHTTP5xx    = "http_5xx"
	ClassACLBlocked = "acl_blocked" // reserved for the deferred Control-API ACL audit; not produced by the netmap vantage
	ClassServe      = "serve"       // tailnet vantage: service is up locally but not published via `tailscale serve`
	ClassContainer  = "container"   // local down -> container problem
)

// Caps for log capture (also enforced agent-side).
const (
	MaxLogLines = 40
	MaxLogBytes = 8 * 1024
)

// Log capture modes — the workspace default ([LogConfig.Mode]) and per-service
// overrides ([LogConfig.Overrides]) both use these.
const (
	LogModeIncident   = "incident"   // capture the tail on a down-signal event
	LogModeOff        = "off"        // never capture (default)
	LogModeContinuous = "continuous" // reserved: rolling capture, not yet implemented
)
