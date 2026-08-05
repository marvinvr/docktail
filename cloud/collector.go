package cloud

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/events"
	"github.com/rs/zerolog"

	"github.com/marvinvr/docktail/cloud/proto"
	"github.com/marvinvr/docktail/docker"
	apptypes "github.com/marvinvr/docktail/types"
)

// agentVersion is reported in Hello. Release builds replace the development
// value with the DockTail image tag via -ldflags.
var agentVersion = "dev"

// restartLoopThreshold is the container RestartCount above which a die is also
// treated as a restart-loop signal.
const restartLoopThreshold = 3

// unmonitoredSnapshotInterval throttles snapshots for a host the cloud reports
// as unmonitored (inactive workspace or past the plan cap). Such a host stops sending
// checks/events/logs entirely and emits only this occasional catalog-teaser
// snapshot plus its heartbeat, until the cloud promotes it again.
const unmonitoredSnapshotInterval = 5 * time.Minute

// Collector is DockTail's cloud reporting module. It implements
// reconciler.Observer: it receives the reconciler's computed services and the
// docker event stream, maps them to wire messages, and streams them over an
// outbound WSS connection it manages itself. It also runs local-vantage checks
// and a heartbeat while connected.
type Collector struct {
	cfg     Config
	docker  *docker.Client
	log     zerolog.Logger
	checker *checker
	tailnet tailnetSource // local netmap reader (serve state + peer liveness); nil ⇒ no tailnet vantage

	fingerprint   string
	hostname      string
	dockerVersion string
	specs         docker.HostSpec

	mu           sync.RWMutex
	conn         *wsConn             // current live connection, or nil when disconnected
	latest       []proto.Service     // last computed snapshot (sent on (re)connect)
	checks       []proto.CheckConfig // cloud-pushed check config
	logMode      string              // workspace default capture mode ("" ⇒ proto.LogModeOff)
	logOverrides map[string]string   // per-service capture mode override (service key -> proto.LogMode*)
	checkFails   map[string]int      // consecutive local-check failures per service key, for incident log capture
	cfgVer       int
	unmonitored  bool      // cloud reports this host inactive/past the plan cap; throttle output
	lastTeaser   time.Time // last throttled teaser snapshot sent while unmonitored

	statsMu      sync.Mutex           // guards prevCPU + prevCPUOther
	prevCPU      map[string]cpuSample // last CPU counters per service container, for % deltas
	prevCPUOther map[string]cpuSample // same, for non-docktail containers (kept separate so neither prunes the other)

	hostMx         *hostMetricsReader // whole-host /proc + /sys vitals reader
	hostMetricsCap bool               // host CPU/mem readable here → advertise + run metricsLoop
	hostTempCap    bool               // temperature sensors detected → advertise host_temp
	loadNodeScoped bool               // /proc loadavg is the physical node's, not this CT's → don't report it
}

// cpuSample is the previous CPU counter reading kept per container. Docker
// reports cumulative counters, so a percentage is the delta between two reads.
type cpuSample struct {
	total  uint64
	system uint64
}

// containerStats is the per-container live usage attached to each wire Service.
// cpuPercent is nil when unknown (not running, or the first sample with no prior
// reading); a non-nil 0 is a genuine idle reading. memUsage/memLimit are zero
// only when unsampled (a running container's working set is never 0).
type containerStats struct {
	cpuPercent *float64
	memUsage   int64
	memLimit   int64
}

// NewCollector builds a Collector, reading the host fingerprint (docker engine
// ID) and versions up front. Returns an error only if the engine ID can't be
// read — without it there is no stable host identity. ts is the local tailscale
// daemon reader used to source the tailnet vantage from the host's own netmap;
// pass nil (or a client with no tailnet) to run without the tailnet vantage.
func NewCollector(ctx context.Context, cfg Config, dc *docker.Client, ts tailnetSource, logger zerolog.Logger) (*Collector, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("cloud config: %w", err)
	}
	fp, err := dc.EngineID(ctx)
	if err != nil {
		return nil, err
	}
	hmr := newHostMetricsReader()
	specs := dc.HostSpecs(ctx)
	c := &Collector{
		cfg:            cfg,
		docker:         dc,
		log:            logger,
		checker:        newChecker(),
		tailnet:        ts,
		fingerprint:    fp,
		hostname:       dc.Hostname(ctx),
		dockerVersion:  dc.ServerVersion(ctx),
		specs:          specs,
		logOverrides:   map[string]string{},
		checkFails:     map[string]int{},
		prevCPU:        map[string]cpuSample{},
		prevCPUOther:   map[string]cpuSample{},
		hostMx:         hmr,
		hostMetricsCap: hmr.available(),
		hostTempCap:    hmr.tempAvailable(),
	}
	// On a Proxmox LXC the agent's /proc is the physical node's, so loadavg is the
	// whole node's load — meaningless against the CT's (smaller) core count, where
	// it reads as a permanent >100% even on an idle container. Detect that by
	// comparing the node's logical CPUs (/proc/stat) against docker's NCPU (the
	// CT's cores); when the node has more, this host's loadavg isn't the CT's, so
	// suppress it (the cloud then shows "—" instead of a misleading ratio). The
	// CT's own load isn't readable from inside the nested container.
	if specs.CPUCores > 0 && physicalCPUCount() > specs.CPUCores {
		c.loadNodeScoped = true
	}
	return c, nil
}

// Fingerprint is the docker engine ID used as the host identity.
func (c *Collector) Fingerprint() string { return c.fingerprint }

// ---- reconciler.Observer -------------------------------------------------

// OnReconcile receives the reconciler's freshly computed services, enriches them
// with runtime detail, stores them, and (if connected) sends a snapshot.
func (c *Collector) OnReconcile(ctx context.Context, services []*apptypes.ContainerService) {
	built := c.buildServices(ctx, services)

	c.mu.Lock()
	c.latest = built
	conn := c.conn
	unmonitored := c.unmonitored
	c.mu.Unlock()

	// Unmonitored: keep latest fresh for a future promotion but stay off the wire
	// here — the throttled discover loop owns the occasional teaser snapshot.
	if conn != nil && !unmonitored {
		// Partial: the reconciler only sees running services, so this refreshes
		// enrichment (FQDN/funnel) but must not drive removals — otherwise a
		// stopped container would be mistaken for a deleted one.
		c.send(conn, proto.TypeSnapshot, proto.Snapshot{Services: built, Full: false})
		c.log.Debug().Int("services", len(built)).Msg("cloud: snapshot sent")
	}
}

// OnEvent maps a docker event to wire events and (if connected) sends them,
// capturing a log excerpt for opted-in services on down signals.
func (c *Collector) OnEvent(ctx context.Context, msg events.Message) {
	evs := c.mapEvents(ctx, msg)
	if len(evs) == 0 {
		return
	}
	c.mu.RLock()
	conn := c.conn
	unmonitored := c.unmonitored
	c.mu.RUnlock()
	// Unmonitored: the cloud drops events and log excerpts, so don't send them
	// (and skip the log capture work entirely).
	if conn == nil || unmonitored {
		return
	}
	for _, ev := range evs {
		c.send(conn, proto.TypeEvent, ev)
		c.maybeCaptureLogs(ctx, conn, ev)
	}
}

// ---- snapshot building ---------------------------------------------------

// buildServices maps reconciler ContainerService values to wire Services,
// enriching each with a single inspect + stats sample per distinct container.
func (c *Collector) buildServices(ctx context.Context, services []*apptypes.ContainerService) []proto.Service {
	type enriched struct {
		info  docker.CloudInfo
		stats containerStats
	}
	cache := make(map[string]enriched)
	out := make([]proto.Service, 0, len(services))
	for _, cs := range services {
		if cs == nil {
			continue
		}
		e, ok := cache[cs.ContainerID]
		if !ok {
			if ci, err := c.docker.InspectCloud(ctx, cs.ContainerID); err == nil {
				e.info = ci
			}
			e.stats = c.sampleStats(ctx, cs.ContainerID, e.info.State)
			cache[cs.ContainerID] = e
		}
		out = append(out, toService(cs, e.info, e.stats))
	}
	present := make(map[string]struct{}, len(cache))
	for id := range cache {
		present[id] = struct{}{}
	}
	c.pruneStats(present)
	return out
}

// sampleStats reads a one-shot docker stats sample for a running container and
// turns it into a current-value usage reading. Memory is taken as-is; CPU% is
// the delta of cumulative counters against this container's previous sample —
// zero on the first sample, after which it self-corrects. Best-effort: a
// non-running container or any error yields a zero reading, which the cloud
// stores as "unknown" (NULL).
func (c *Collector) sampleStats(ctx context.Context, containerID, state string) containerStats {
	return c.sampleStatsWith(ctx, containerID, state, c.prevCPU)
}

// sampleStatsWith is sampleStats against a caller-supplied previous-CPU map, so
// monitored services (c.prevCPU) and non-docktail containers (c.prevCPUOther)
// keep independent CPU-delta history and never prune each other's samples.
func (c *Collector) sampleStatsWith(ctx context.Context, containerID, state string, prevCPU map[string]cpuSample) containerStats {
	if containerID == "" || state != "running" {
		return containerStats{}
	}
	s, err := c.docker.ContainerStats(ctx, containerID)
	if err != nil {
		c.log.Debug().Err(err).Str("container", containerID).Msg("cloud: stats sample failed")
		return containerStats{}
	}
	out := containerStats{memUsage: s.MemUsageBytes, memLimit: s.MemLimitBytes}

	c.statsMu.Lock()
	prev, ok := prevCPU[containerID]
	prevCPU[containerID] = cpuSample{total: s.CPUTotalUsage, system: s.CPUSystemUsage}
	c.statsMu.Unlock()

	// A percentage needs two samples. Report it once we have a prior reading and
	// the host system-time advanced — *including* a genuine 0% for an idle
	// container (cpuDelta 0), which is a real value, not "unknown". The formula
	// is self-normalizing on the system-time delta, so a varying interval between
	// samples (reconcile vs. discovery loop) is fine. Skip only when the
	// container's own counter went backwards (a restart reset it); the next tick
	// re-syncs against the fresh prev.
	if ok && s.CPUSystemUsage > prev.system && s.CPUTotalUsage >= prev.total {
		cpuDelta := float64(s.CPUTotalUsage - prev.total)
		sysDelta := float64(s.CPUSystemUsage - prev.system)
		onlineCPUs := float64(s.OnlineCPUs)
		if onlineCPUs == 0 {
			onlineCPUs = 1
		}
		pct := math.Round((cpuDelta/sysDelta)*onlineCPUs*100*100) / 100 // 2 decimals
		out.cpuPercent = &pct
	}
	return out
}

// pruneStats drops previous-CPU samples for containers absent from the latest
// build, keeping the cache bounded to currently-managed containers.
func (c *Collector) pruneStats(present map[string]struct{}) {
	c.pruneStatsMap(present, c.prevCPU)
}

// pruneStatsMap drops previous-CPU samples for containers absent from present,
// from a caller-supplied map (c.prevCPU for services, c.prevCPUOther for
// non-docktail containers).
func (c *Collector) pruneStatsMap(present map[string]struct{}, prevCPU map[string]cpuSample) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	for id := range prevCPU {
		if _, ok := present[id]; !ok {
			delete(prevCPU, id)
		}
	}
}

// toService maps a reconciler ContainerService + docker enrichment onto the wire
// Service. FQDN is left empty: the tailnet vantage is sourced from the host's own
// `tailscale serve` config (see tailnet.go), keyed by service name + port, so the
// cloud does not need the MagicDNS domain to classify serve state.
func toService(cs *apptypes.ContainerService, info docker.CloudInfo, stats containerStats) proto.Service {
	svc := proto.Service{
		Key:            serviceKeyForContainerService(cs),
		ServiceName:    cs.ServiceName,
		ContainerID:    cs.ContainerID,
		ContainerName:  cs.ContainerName,
		Image:          info.Image,
		ImageTag:       info.ImageTag,
		ComposeProject: info.ComposeProject,
		ComposeService: info.ComposeService,
		IPAddress:      cs.IPAddress,
		Port:           cs.Port,
		TargetPort:     cs.TargetPort,
		CheckIP:        cs.MonitorIP,
		CheckPort:      cs.MonitorPort,
		ServiceProto:   cs.ServiceProtocol,
		Protocol:       cs.Protocol,
		Tags:           cs.Tags,
		Networks:       info.Networks,
		State:          info.State,
		DockerHealth:   info.Health,
		RestartCount:   info.RestartCount,
		CPUPercent:     stats.cpuPercent,
		MemUsageBytes:  stats.memUsage,
		MemLimitBytes:  stats.memLimit,
	}
	if cs.FunnelEnabled {
		svc.FunnelEnabled = true
		svc.FunnelPort = firstNonEmpty(cs.FunnelFunnelPort, cs.FunnelPort)
		svc.FunnelProtocol = cs.FunnelProtocol
		svc.FunnelPath = cs.FunnelPath
	}
	return svc
}

func serviceKeyForContainerService(cs *apptypes.ContainerService) string {
	serviceName := strings.TrimSpace(cs.ServiceName)
	if serviceName != "" {
		if port := strings.TrimSpace(cs.Port); port != "" {
			return serviceName + ":" + port
		}
		return serviceName
	}
	containerName := strings.TrimSpace(cs.ContainerName)
	if cs.FunnelEnabled {
		if port := strings.TrimSpace(firstNonEmpty(cs.FunnelFunnelPort, cs.FunnelPort)); port != "" {
			return containerName + ":funnel:" + port
		}
	}
	return containerName
}

// ---- event mapping -------------------------------------------------------

func (c *Collector) mapEvents(ctx context.Context, msg events.Message) []proto.Event {
	attrs := msg.Actor.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	bases := c.eventBases(msg, attrs)
	if len(bases) == 0 {
		return nil
	}
	action := string(msg.Action)

	switch {
	case msg.Action == events.ActionDie:
		out := make([]proto.Event, 0, len(bases)*2)
		for _, base := range bases {
			ev := base
			ev.Kind = proto.EventDie
			if code, ok := atoiPtr(attrs["exitCode"]); ok {
				ev.ExitCode = code
			}
			out = append(out, ev)
		}
		if rc := c.docker.RestartCount(ctx, msg.Actor.ID); rc > restartLoopThreshold {
			for _, base := range bases {
				loop := base
				loop.Kind = proto.EventRestartLoop
				loop.RestartCount = rc
				out = append(out, loop)
			}
		}
		return out
	case msg.Action == events.ActionOOM:
		return eventsWithKind(bases, proto.EventOOM)
	case msg.Action == events.ActionStart:
		return eventsWithKind(bases, proto.EventStart)
	case msg.Action == events.ActionStop || msg.Action == events.ActionRestart:
		out := eventsWithKind(bases, proto.EventStop)
		if msg.Action == events.ActionRestart {
			for i := range out {
				out[i].Message = "restart"
			}
		}
		return out
	case strings.HasPrefix(action, "health_status"):
		out := eventsWithKind(bases, proto.EventHealthStatus)
		for i := range out {
			out[i].HealthStatus = healthStatusFromEvent(action, attrs)
		}
		return out
	}
	return nil
}

func eventsWithKind(bases []proto.Event, kind proto.EventKind) []proto.Event {
	out := make([]proto.Event, 0, len(bases))
	for _, base := range bases {
		ev := base
		ev.Kind = kind
		out = append(out, ev)
	}
	return out
}

func (c *Collector) eventBases(msg events.Message, attrs map[string]string) []proto.Event {
	keys := c.serviceKeysForEvent(msg, attrs)
	if len(keys) == 0 {
		return nil
	}
	out := make([]proto.Event, 0, len(keys))
	for _, key := range keys {
		out = append(out, proto.Event{
			ContainerID:   msg.Actor.ID,
			ContainerName: attrs["name"],
			ServiceKey:    key,
			OccurredAt:    eventMillis(msg),
		})
	}
	return out
}

func (c *Collector) serviceKeysForEvent(msg events.Message, attrs map[string]string) []string {
	c.mu.RLock()
	latest := c.latest
	c.mu.RUnlock()

	var keys []string
	seen := map[string]struct{}{}
	for _, svc := range latest {
		if !sameContainer(msg.Actor.ID, attrs["name"], svc.ContainerID, svc.ContainerName) {
			continue
		}
		if strings.TrimSpace(svc.Key) == "" {
			continue
		}
		if _, ok := seen[svc.Key]; ok {
			continue
		}
		seen[svc.Key] = struct{}{}
		keys = append(keys, svc.Key)
	}
	if len(keys) > 0 {
		return keys
	}

	if key := serviceKeyFromAttrs(attrs); key != "" {
		return []string{key}
	}
	return nil
}

func sameContainer(eventID, eventName, serviceID, serviceName string) bool {
	eventID = strings.TrimSpace(eventID)
	serviceID = strings.TrimSpace(serviceID)
	if eventID != "" && serviceID != "" && (strings.HasPrefix(eventID, serviceID) || strings.HasPrefix(serviceID, eventID)) {
		return true
	}
	eventName = strings.TrimPrefix(strings.TrimSpace(eventName), "/")
	serviceName = strings.TrimPrefix(strings.TrimSpace(serviceName), "/")
	return eventName != "" && serviceName != "" && eventName == serviceName
}

func serviceKeyFromAttrs(attrs map[string]string) string {
	if sn := strings.TrimSpace(attrs[apptypes.LabelService]); sn != "" {
		if port := servicePortFromAttrs(attrs); port != "" {
			return sn + ":" + port
		}
		return sn
	}
	return attrs["name"]
}

func servicePortFromAttrs(attrs map[string]string) string {
	if port := strings.TrimSpace(attrs[apptypes.LabelPort]); port != "" {
		return port
	}
	if strings.TrimSpace(attrs[apptypes.LabelTarget]) == "" {
		return ""
	}
	if strings.TrimSpace(attrs[apptypes.LabelServiceProtocol]) == "https" {
		return "443"
	}
	return "80"
}

func healthStatusFromEvent(action string, attrs map[string]string) string {
	if hs := strings.TrimSpace(attrs["health_status"]); hs != "" {
		return hs
	}
	if idx := strings.Index(action, ":"); idx >= 0 {
		return strings.TrimSpace(action[idx+1:])
	}
	return ""
}

func eventMillis(msg events.Message) int64 {
	if msg.TimeNano > 0 {
		return msg.TimeNano / 1_000_000
	}
	return msg.Time * 1000
}

func atoiPtr(s string) (*int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, false
	}
	return &n, true
}

// maybeCaptureLogs captures + sends a log excerpt for docker down-signal events
// only when the service's effective capture mode enables it.
// health_status is captured only on the unhealthy transition — the only one the
// cloud opens an incident for; healthy/starting transitions carry no incident.
func (c *Collector) maybeCaptureLogs(ctx context.Context, conn *wsConn, ev proto.Event) {
	switch ev.Kind {
	case proto.EventDie, proto.EventOOM, proto.EventRestartLoop:
	case proto.EventHealthStatus:
		if !strings.EqualFold(ev.HealthStatus, "unhealthy") {
			return
		}
	default:
		return
	}
	c.captureAndSend(ctx, conn, ev.ServiceKey, ev.ContainerID)
}

// incidentCaptureThreshold mirrors the cloud health engine's local-failure
// debounce (DefaultDebounce): the cloud opens a container incident on the Nth
// consecutive local failure, so the agent captures the log tail on that same
// edge. If the cloud's debounce differs, the excerpt arrives before the incident
// opens and the cloud back-links it (AttachOrphanLogExcerpts), so this need not
// match exactly — it only keeps the common case attaching directly.
const incidentCaptureThreshold = 2

// captureOnCheckFailures captures a log excerpt for any service whose local
// checks have just crossed the debounce threshold, mirroring the docker-event
// path for probe-driven incidents (a running container that fails its probe
// emits no docker event, so this is the only signal that would carry logs).
// Capture fires exactly once per failing episode — on the threshold edge — and
// the per-service streak resets on the next OK. Must be called after the
// check_results frame is sent so the excerpt arrives after the cloud has had a
// chance to open the incident.
func (c *Collector) captureOnCheckFailures(ctx context.Context, conn *wsConn, services []proto.Service, results []proto.CheckResult) {
	byKey := make(map[string]proto.Service, len(services))
	for _, s := range services {
		byKey[s.Key] = s
	}
	for _, r := range results {
		if r.ServiceKey == "" {
			continue
		}
		c.mu.Lock()
		if r.OK {
			delete(c.checkFails, r.ServiceKey)
			c.mu.Unlock()
			continue
		}
		n := c.checkFails[r.ServiceKey] + 1
		c.checkFails[r.ServiceKey] = n
		c.mu.Unlock()
		if n != incidentCaptureThreshold {
			continue // below the edge, or already captured this episode
		}
		if svc, ok := byKey[r.ServiceKey]; ok {
			c.captureAndSend(ctx, conn, r.ServiceKey, svc.ContainerID)
		}
	}
	c.pruneCheckFails(byKey)
}

// pruneCheckFails drops failure streaks for service keys no longer present in the
// current snapshot, keeping the tracker bounded to live services.
func (c *Collector) pruneCheckFails(present map[string]proto.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.checkFails {
		if _, ok := present[k]; !ok {
			delete(c.checkFails, k)
		}
	}
}

// captureAndSend captures a capped log excerpt for a service and sends it, unless
// the service's effective capture mode is off or there is no container to read.
// Shared by the docker-event and local-check down paths. Best-effort: a capture
// error is logged at debug and dropped.
func (c *Collector) captureAndSend(ctx context.Context, conn *wsConn, serviceKey, containerID string) {
	if containerID == "" || c.logModeFor(serviceKey) == proto.LogModeOff {
		return
	}
	excerpt, err := c.captureLogs(ctx, serviceKey, containerID)
	if err != nil || excerpt == nil {
		return
	}
	c.send(conn, proto.TypeLogExcerpt, excerpt)
	c.log.Debug().Str("service", serviceKey).Int("lines", len(excerpt.Lines)).Msg("cloud: log excerpt sent")
}

// ---- connection lifecycle ------------------------------------------------

// Run is the reconnect loop. It blocks until ctx is cancelled. Each iteration
// dials, performs the hello handshake, and (on accept) serves until the
// connection drops, then backs off — unless the rejection is terminal.
func (c *Collector) Run(ctx context.Context) {
	bo := newBackoff()
	for ctx.Err() == nil {
		if c.session(ctx, bo) {
			return // terminal rejection
		}
		if ctx.Err() != nil {
			return
		}
		d := bo.next()
		c.log.Info().Dur("backoff", d).Msg("cloud: reconnecting after backoff")
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}
}

// session runs one connection lifetime; stop=true means give up entirely.
func (c *Collector) session(ctx context.Context, bo *backoff) (stop bool) {
	dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
	conn, err := dial(dialCtx, c.cfg.URL, c.cfg.Key, c.log)
	dialCancel()
	if err != nil {
		var de *dialError
		if asDialError(err, &de) && (de.statusCode == 401 || de.statusCode == 403) {
			c.log.Error().Int("status", de.statusCode).Msg("cloud: connection rejected (auth) — stopping")
			return true
		}
		c.log.Warn().Err(err).Msg("cloud: dial failed")
		return false
	}

	ackCh := make(chan proto.HelloAck, 1)
	h := handlers{
		onHelloAck: func(ack proto.HelloAck) {
			select {
			case ackCh <- ack:
			default:
			}
		},
		onConfig: func(cfg proto.Config) { c.applyConfig(cfg) },
	}

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	runDone := make(chan error, 1)
	go func() { runDone <- conn.run(connCtx, h) }()

	if !c.sendHello(connCtx, conn) {
		connCancel()
		<-runDone
		return false
	}

	select {
	case <-connCtx.Done():
		<-runDone
		return false
	case err := <-runDone:
		if err != nil {
			c.log.Warn().Err(err).Msg("cloud: connection closed before hello_ack")
		}
		return false
	case ack := <-ackCh:
		if !ack.Accepted {
			if terminalReject(ack.Reason) {
				c.log.Error().Str("reason", string(ack.Reason)).Msg("cloud: hello rejected (terminal) — stopping")
				connCancel()
				<-runDone
				return true
			}
			if ack.Reason == proto.RejectEnrollmentClosed {
				// An operator can reopen the key's enrollment window, so keep
				// retrying automatically without flooding the logs while closed.
				bo.slow()
			}
			c.log.Warn().Str("reason", string(ack.Reason)).Msg("cloud: hello rejected — will retry")
			connCancel()
			<-runDone
			return false
		}
		c.log.Info().Str("host_id", ack.HostID).Int("config_version", ack.ConfigVersion).Msg("cloud: connected and accepted")
	case <-time.After(15 * time.Second):
		c.log.Warn().Msg("cloud: timed out waiting for hello_ack")
		connCancel()
		<-runDone
		return false
	}

	// Accepted. Publish the connection, send an immediate snapshot from the last
	// reconcile, and start the heartbeat + check loops.
	bo.reset()
	c.setConn(conn)
	c.sendCurrentSnapshot(conn)
	go c.discoverLoop(connCtx, conn)
	go c.heartbeatLoop(connCtx, conn)
	go c.checkLoop(connCtx, conn)
	if c.hostMetricsCap {
		go c.metricsLoop(connCtx, conn)
	}
	if c.tailnet != nil {
		go c.tailnetLoop(connCtx, conn)
	}

	err = <-runDone
	c.clearConn(conn)
	if err != nil {
		c.log.Warn().Err(err).Msg("cloud: connection closed")
	}
	return false
}

func (c *Collector) heartbeatLoop(ctx context.Context, conn *wsConn) {
	ticker := time.NewTicker(proto.HeartbeatInterval * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.send(conn, proto.TypeHeartbeat, proto.Heartbeat{Uptime: conn.uptime()})
		}
	}
}

// metricsLoop samples whole-host vitals from /proc and /sys on the heartbeat
// cadence and sends a host_metrics frame. Started only when the host_metrics
// capability is available; stays silent while unmonitored (the cloud drops the
// frame anyway).
func (c *Collector) metricsLoop(ctx context.Context, conn *wsConn) {
	ticker := time.NewTicker(proto.HeartbeatInterval * time.Second)
	defer ticker.Stop()
	c.sampleAndSendMetrics(conn)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sampleAndSendMetrics(conn)
		}
	}
}

func (c *Collector) sampleAndSendMetrics(conn *wsConn) {
	// Always sample so the CPU-delta baseline stays fresh even while unmonitored;
	// only the send is gated (the cloud drops host_metrics for unmonitored hosts),
	// so the first sample after a promotion isn't averaged over the whole gap.
	m := c.hostMx.sample()
	// On a Proxmox LXC the sampled loadavg is the physical node's, not the CT's, so
	// drop it rather than report a node load against the container's core count.
	if c.loadNodeScoped {
		m.Load1, m.Load5, m.Load15 = nil, nil, nil
	}
	c.mu.RLock()
	unmonitored := c.unmonitored
	c.mu.RUnlock()
	if unmonitored {
		return
	}
	c.send(conn, proto.TypeHostMetrics, m)
}

func (c *Collector) checkLoop(ctx context.Context, conn *wsConn) {
	ticker := time.NewTicker(c.cfg.CheckInterval)
	defer ticker.Stop()
	c.runChecks(ctx, conn)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runChecks(ctx, conn)
		}
	}
}

func (c *Collector) runChecks(ctx context.Context, conn *wsConn) {
	c.mu.RLock()
	services := c.latest
	configs := c.checks
	unmonitored := c.unmonitored
	c.mu.RUnlock()
	// Unmonitored: the cloud drops check_results, so don't even run the local
	// probes.
	if unmonitored || len(services) == 0 {
		return
	}
	results := c.checker.run(ctx, services, configs)
	// Tailnet-vantage results from the host's own `tailscale serve` config (the
	// netmap-sourced vantage — no credentials). nil when there is no tailnet, so
	// the cloud leaves the tailnet vantage not_configured.
	tnResults := c.tailnetResults(ctx, services)
	frame := results
	if len(tnResults) > 0 {
		frame = make([]proto.CheckResult, 0, len(results)+len(tnResults))
		frame = append(frame, results...)
		frame = append(frame, tnResults...)
	}
	if len(frame) == 0 {
		return
	}
	c.send(conn, proto.TypeCheckResults, proto.CheckResults{Results: frame})
	c.log.Debug().Int("results", len(results)).Int("tailnet_results", len(tnResults)).Msg("cloud: check results sent")
	// Capture logs for services that just crossed the down-debounce edge — the
	// probe-driven counterpart to maybeCaptureLogs. Sent after check_results so
	// the excerpt lands once the cloud has opened the incident. Local results
	// only: a not-published tailnet result must not inflate the local fail streak.
	c.captureOnCheckFailures(ctx, conn, services, results)
}

func (c *Collector) sendCurrentSnapshot(conn *wsConn) {
	c.mu.RLock()
	services := c.latest
	c.mu.RUnlock()
	if services == nil {
		services = []proto.Service{}
	}
	// Partial: the cached snapshot may be stale/running-only. discoverLoop sends
	// an authoritative full snapshot immediately after connect, so this initial
	// push must not drive removals.
	c.send(conn, proto.TypeSnapshot, proto.Snapshot{Services: services, Full: false})
}

// discoverLoop lists enabled containers straight from Docker on a ticker and
// pushes a snapshot. This makes cloud reporting self-sufficient: it does not
// depend on the reconciler's OnReconcile callback, which only fires after a
// *successful* tailscale serve reconcile. A host with no tailnet (or a failing
// one) therefore still reports its services to the cloud. OnReconcile remains
// wired as an additional, event-driven refresh when the reconcile does succeed.
func (c *Collector) discoverLoop(ctx context.Context, conn *wsConn) {
	ticker := time.NewTicker(c.cfg.CheckInterval)
	defer ticker.Stop()
	c.scanAndSnapshot(ctx, conn)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.scanAndSnapshot(ctx, conn)
		}
	}
}

// snapshotDue reports whether this discover tick should scan and send. A
// monitored host always sends; an unmonitored host sends only every
// unmonitoredSnapshotInterval (a low-rate catalog teaser), recording the send
// time so subsequent ticks back off.
func (c *Collector) snapshotDue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.unmonitored {
		return true
	}
	if time.Since(c.lastTeaser) < unmonitoredSnapshotInterval {
		return false
	}
	c.lastTeaser = time.Now()
	return true
}

// scanAndSnapshot discovers enabled containers via Docker, caches the built
// services as the latest snapshot, and sends it.
func (c *Collector) scanAndSnapshot(ctx context.Context, conn *wsConn) {
	// Throttle the whole scan when unmonitored — skip the docker inspect/stats
	// work too, not just the send.
	if !c.snapshotDue() {
		return
	}
	containers, err := c.docker.GetCloudContainers(ctx)
	if err != nil {
		c.log.Warn().Err(err).Msg("cloud: container discovery failed")
		return
	}
	built := c.buildServices(ctx, containers)
	c.mu.Lock()
	c.latest = built
	c.mu.Unlock()
	// Full: self-discovery includes stopped containers, so it is authoritative
	// for presence — the cloud uses it to detect removed services.
	c.send(conn, proto.TypeSnapshot, proto.Snapshot{Services: built, Full: true})
	c.log.Debug().Int("services", len(built)).Msg("cloud: snapshot sent (self-discovered)")

	// Also report the host's non-docktail containers (metadata-only inventory).
	c.scanAndSendOtherContainers(ctx, conn)
}

// scanAndSendOtherContainers discovers the host's NON-docktail containers and
// sends them as a full, authoritative `containers` frame. It is metadata-only
// (no checks/events) and shares the discover loop's cadence. Skipped while
// unmonitored: the cloud drops these for an over-cap host and its detail page
// hides it, so there is nothing to show.
func (c *Collector) scanAndSendOtherContainers(ctx context.Context, conn *wsConn) {
	c.mu.RLock()
	unmonitored := c.unmonitored
	c.mu.RUnlock()
	if unmonitored {
		return
	}
	containers, err := c.docker.GetOtherContainers(ctx)
	if err != nil {
		c.log.Warn().Err(err).Msg("cloud: other-container discovery failed")
		return
	}
	built := c.buildContainers(ctx, containers)
	c.send(conn, proto.TypeContainers, proto.Containers{Containers: built, Full: true})
	c.log.Debug().Int("containers", len(built)).Msg("cloud: containers sent (non-docktail)")
}

// buildContainers maps docker OtherContainer values onto wire Containers,
// sampling a one-shot stats reading per running container against prevCPUOther
// (kept separate from services' prevCPU so the two never prune each other).
func (c *Collector) buildContainers(ctx context.Context, containers []docker.OtherContainer) []proto.Container {
	out := make([]proto.Container, 0, len(containers))
	present := make(map[string]struct{}, len(containers))
	for _, oc := range containers {
		st := c.sampleStatsWith(ctx, oc.ID, oc.State, c.prevCPUOther)
		present[oc.ID] = struct{}{}
		out = append(out, proto.Container{
			ContainerID:    oc.ID,
			IsAgent:        oc.IsAgent,
			Name:           oc.Name,
			Image:          oc.Image,
			ImageTag:       oc.ImageTag,
			State:          oc.State,
			Status:         oc.Status,
			Health:         oc.Health,
			ComposeProject: oc.ComposeProject,
			ComposeService: oc.ComposeService,
			Ports:          oc.Ports,
			CreatedAt:      oc.CreatedAt,
			CPUPercent:     st.cpuPercent,
			MemUsageBytes:  st.memUsage,
			MemLimitBytes:  st.memLimit,
		})
	}
	c.pruneStatsMap(present, c.prevCPUOther)
	return out
}

// ---- helpers -------------------------------------------------------------

func (c *Collector) sendHello(ctx context.Context, conn *wsConn) bool {
	caps := []string{"http_checks", "log_capture", "container_stats", "container_inventory"}
	if c.hostMetricsCap {
		caps = append(caps, "host_metrics")
	}
	if c.hostTempCap {
		caps = append(caps, "host_temp")
	}
	hello := proto.Hello{
		ProtocolVersion: proto.ProtocolVersion,
		Fingerprint:     c.fingerprint,
		TailscaleNodeID: c.tailnetSelfID(ctx),
		Hostname:        c.hostname,
		AgentVersion:    agentVersion,
		DockerVersion:   c.dockerVersion,
		Capabilities:    caps,
		OS:              c.specs.OS,
		KernelVersion:   c.specs.KernelVersion,
		Arch:            c.specs.Arch,
		CPUCores:        c.specs.CPUCores,
		MemTotalBytes:   c.specs.MemTotalBytes,
	}
	env, err := proto.Encode(proto.TypeHello, nowMillis(), hello)
	if err != nil {
		c.log.Error().Err(err).Msg("cloud: encode hello failed")
		return false
	}
	return conn.sendFrame(env)
}

func (c *Collector) send(conn *wsConn, t proto.MessageType, msg any) {
	env, err := proto.Encode(t, nowMillis(), msg)
	if err != nil {
		c.log.Warn().Err(err).Str("type", string(t)).Msg("cloud: encode failed")
		return
	}
	conn.sendFrame(env)
}

func (c *Collector) setConn(conn *wsConn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *Collector) clearConn(conn *wsConn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
}

func (c *Collector) applyConfig(cfg proto.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg.Version < c.cfgVer {
		return
	}
	c.cfgVer = cfg.Version
	checks, rejectedChecks := proto.SanitizeCheckConfigs(cfg.Checks)
	c.checks = checks

	// On the monitored→unmonitored transition, zero the teaser timer so the next
	// discover tick emits one snapshot immediately (populating the catalog teaser)
	// before settling into the throttled cadence.
	if cfg.Unmonitored && !c.unmonitored {
		c.lastTeaser = time.Time{}
	}
	c.unmonitored = cfg.Unmonitored

	mode := cfg.Logs.Mode
	overrides, rejectedOverrides := proto.SanitizeLogOverrides(cfg.Logs.Overrides)
	legacyLogOptIn := cfg.LogOptIn //nolint:staticcheck // Required for compatibility with older control planes.
	if mode == "" && len(overrides) == 0 && len(legacyLogOptIn) > 0 {
		// Legacy config from an older cloud: only the listed service keys
		// captured (on incident). Map that onto the mode model so behaviour is
		// unchanged against an older control plane.
		mode = proto.LogModeOff
		overrides = make(map[string]string, len(legacyLogOptIn))
		for _, k := range legacyLogOptIn {
			overrides[k] = proto.LogModeIncident
		}
		var legacyRejected int
		overrides, legacyRejected = proto.SanitizeLogOverrides(overrides)
		rejectedOverrides += legacyRejected
	}
	mode = proto.SafeLogMode(mode)
	c.logMode = mode
	c.logOverrides = overrides

	c.log.Info().
		Int("version", cfg.Version).
		Int("checks", len(checks)).
		Int("rejected_checks", rejectedChecks).
		Str("log_mode", mode).
		Int("log_overrides", len(overrides)).
		Int("rejected_log_overrides", rejectedOverrides).
		Bool("unmonitored", cfg.Unmonitored).
		Msg("cloud: applied config")
}

// logModeFor returns the effective capture mode for a service key: its override
// when set, else the workspace default, else the built-in default (off).
func (c *Collector) logModeFor(serviceKey string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if m := c.logOverrides[serviceKey]; m != "" {
		return m
	}
	if c.logMode == "" {
		return proto.LogModeOff
	}
	return c.logMode
}

func terminalReject(reason proto.RejectCode) bool {
	switch reason {
	case proto.RejectInvalidKey, proto.RejectBlocked, proto.RejectProtocolMismatch:
		return true
	default:
		return false
	}
}

func asDialError(err error, target **dialError) bool {
	for err != nil {
		if de, ok := err.(*dialError); ok {
			*target = de
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
