package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/rs/zerolog/log"

	apptypes "github.com/marvinvr/docktail/types"
)

// This file adds read-only helpers used by the optional cloud module
// (see ../cloud). They reuse the same docker client connection rather than
// opening a second one, and never affect reconciliation behavior. None of these
// are used unless DOCKTAIL_CLOUD_KEY is set.

// CloudInfo is the runtime enrichment the cloud catalog needs that the
// reconciler's ContainerService does not already carry.
type CloudInfo struct {
	Image          string
	ImageTag       string
	State          string // running/exited/restarting/paused/created
	Health         string // healthy/unhealthy/starting (empty if no healthcheck)
	RestartCount   int
	ComposeProject string
	ComposeService string
	Networks       []string
}

// GetCloudContainers lists docktail-managed containers for cloud reporting,
// INCLUDING stopped/exited ones — unlike GetEnabledContainers, which is
// running-only because the serve reconciler must never target a dead container.
// Reporting stopped containers lets the cloud render them as down rather than
// dropping them, and reserves "removed" for containers that truly leave Docker.
//
// Running containers are parsed at full fidelity (multiple/indexed services and
// funnel included). A non-running container whose live parse fails — direct mode
// needs a running IP — falls back to a minimal entry keyed by the same primary
// service name it carries when running, so it stays in the catalog as a down
// service instead of being seen as removed. Limitation: a stopped container that
// declares multiple indexed services reports only its primary service until it
// is running again.
func (c *Client) GetCloudContainers(ctx context.Context) ([]*apptypes.ContainerService, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var services []*apptypes.ContainerService
	for _, cont := range containers {
		if !isManagedContainer(cont.Labels) {
			continue
		}

		parsed, perr := c.parseContainer(ctx, cont.ID, cont.Labels)
		if perr == nil {
			services = append(services, parsed...)
			continue
		}

		name := ""
		if len(cont.Names) > 0 {
			name = strings.TrimPrefix(cont.Names[0], "/")
		}
		// A *running* container that fails to parse is a genuine problem — skip it,
		// matching GetEnabledContainers. A non-running one is expected to fail
		// (e.g. direct mode has no live IP); keep a minimal down entry so the cloud
		// still sees the service.
		if cont.State == "running" {
			log.Warn().Err(perr).Str("container", name).Msg("cloud: failed to parse running container, skipping")
			continue
		}
		services = append(services, c.stoppedCloudServices(cont.ID, name, cont.Labels)...)
	}
	return services, nil
}

// OtherContainer is a NON-docktail container as seen for cloud reporting:
// limited, read-only metadata only. It carries no exec/deploy surface and is
// never health-checked — the cloud renders it as plain inventory alongside the
// monitored services.
type OtherContainer struct {
	ID             string // short container id — identity within the host
	IsAgent        bool   // this container is running the reporting DockTail agent
	Name           string
	Image          string
	ImageTag       string
	State          string // running/exited/restarting/paused/created
	Status         string // human status line, e.g. "Up 3 hours (healthy)"
	Health         string // healthy/unhealthy/starting (best-effort, parsed from Status)
	ComposeProject string
	ComposeService string
	Ports          []string
	CreatedAt      int64 // unix seconds the container was created
}

// GetOtherContainers lists every container that is NOT a docktail-managed
// service (neither docktail.enable nor docktail.funnel.enable set), INCLUDING
// stopped ones, for the cloud's container-inventory view. It is read-only and
// builds each entry straight from the container-list summary — no per-container
// inspect — so it stays cheap even on a busy host. Used only by the cloud module
// (DOCKTAIL_CLOUD_KEY set); docktail-managed containers are reported separately
// by GetCloudContainers as services.
func (c *Client) GetOtherContainers(ctx context.Context) ([]OtherContainer, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	selfID := ownContainerID()
	out := make([]OtherContainer, 0, len(containers))
	for _, cont := range containers {
		if isManagedContainer(cont.Labels) {
			continue // a docktail service — reported via GetCloudContainers
		}
		name := ""
		if len(cont.Names) > 0 {
			name = strings.TrimPrefix(cont.Names[0], "/")
		}
		image, tag := splitImageTag(cont.Image)
		oc := OtherContainer{
			ID:        shortContainerID(cont.ID),
			IsAgent:   selfID != "" && cont.ID == selfID,
			Name:      name,
			Image:     image,
			ImageTag:  tag,
			State:     cont.State,
			Status:    cont.Status,
			Health:    healthFromStatus(cont.Status),
			Ports:     formatContainerPorts(cont.Ports),
			CreatedAt: cont.Created,
		}
		if cont.Labels != nil {
			oc.ComposeProject = cont.Labels["com.docker.compose.project"]
			oc.ComposeService = cont.Labels["com.docker.compose.service"]
		}
		out = append(out, oc)
	}
	return out, nil
}

// healthFromStatus best-effort extracts a docker healthcheck state from the
// human status line the container-list summary reports (e.g. "Up 3 hours
// (healthy)"), since the summary has no dedicated health field. Empty when the
// container declares no healthcheck.
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "health: starting"):
		return "starting"
	default:
		return ""
	}
}

// formatContainerPorts renders a container's port mappings as stable, de-duped
// strings ("0.0.0.0:8080->80/tcp" when published, else "80/tcp"). Docker lists a
// mapping once per host IP (v4 + v6), so identical strings collapse to one.
func formatContainerPorts(ports []container.Port) []string {
	if len(ports) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ports))
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		var s string
		if p.PublicPort != 0 {
			ip := p.IP
			if ip == "" {
				ip = "0.0.0.0"
			}
			s = fmt.Sprintf("%s:%d->%d/%s", ip, p.PublicPort, p.PrivatePort, p.Type)
		} else {
			s = fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (c *Client) stoppedCloudServices(containerID, containerName string, labels map[string]string) []*apptypes.ContainerService {
	id := shortContainerID(containerID)
	tags := c.cloudTags(labels, containerName)
	var services []*apptypes.ContainerService

	if isServiceEnabled(labels) {
		if svc := cloudServiceFromLabels(id, containerID, containerName, labels, "", tags); svc != nil {
			services = append(services, svc)
		}
		for _, idx := range indexedServiceNumbers(labels) {
			if svc := cloudServiceFromLabels(id, containerID, containerName, labels, fmt.Sprintf("docktail.service.%d.", idx), tags); svc != nil {
				services = append(services, svc)
			}
		}
	}

	if !isFunnelEnabled(labels) {
		return services
	}

	funnelPort := labels[apptypes.LabelFunnelPort]
	if funnelPort == "" {
		return services
	}
	funnelProtocol := labels[apptypes.LabelFunnelProtocol]
	if funnelProtocol == "" {
		funnelProtocol = "https"
	}
	funnelFunnelPort := labels[apptypes.LabelFunnelFunnelPort]
	if funnelFunnelPort == "" {
		funnelFunnelPort = "443"
	}
	funnelPath, err := parseFunnelPath(labels[apptypes.LabelFunnelPath])
	if err != nil {
		funnelPath = ""
	}

	if len(services) > 0 {
		services[0].FunnelEnabled = true
		services[0].FunnelPort = funnelPort
		services[0].FunnelTargetPort = funnelPort
		services[0].FunnelFunnelPort = funnelFunnelPort
		services[0].FunnelProtocol = funnelProtocol
		services[0].FunnelPath = funnelPath
		return services
	}

	services = append(services, &apptypes.ContainerService{
		ContainerID:      id,
		ContainerName:    containerName,
		ServiceEnabled:   false,
		Tags:             tags,
		FunnelEnabled:    true,
		FunnelPort:       funnelPort,
		FunnelTargetPort: funnelPort,
		FunnelFunnelPort: funnelFunnelPort,
		FunnelProtocol:   funnelProtocol,
		FunnelPath:       funnelPath,
	})
	return services
}

func cloudServiceFromLabels(id, fullID, containerName string, labels map[string]string, prefix string, tags []string) *apptypes.ContainerService {
	serviceName := labels[prefix+"name"]
	if prefix == "" {
		serviceName = labels[apptypes.LabelService]
	}
	if serviceName == "" {
		return nil
	}

	targetPort := labels[prefix+"port"]
	if prefix == "" {
		targetPort = labels[apptypes.LabelTarget]
	}
	if targetPort == "" {
		return &apptypes.ContainerService{
			ContainerID:    id,
			ContainerName:  containerName,
			ServiceEnabled: true,
			ServiceName:    serviceName,
			Tags:           tags,
		}
	}

	servicePortLabel := labels[prefix+"service-port"]
	serviceProtocolLabel := labels[prefix+"service-protocol"]
	targetProtocolLabel := labels[prefix+"protocol"]
	if prefix == "" {
		servicePortLabel = labels[apptypes.LabelPort]
		serviceProtocolLabel = labels[apptypes.LabelServiceProtocol]
		targetProtocolLabel = labels[apptypes.LabelTargetProtocol]
	}
	protocol, servicePort, serviceProtocol, err := resolveProtocols(fullID, targetPort, servicePortLabel, serviceProtocolLabel, targetProtocolLabel)
	if err != nil {
		servicePort = servicePortLabel
		if servicePort == "" {
			servicePort = targetPort
		}
	}
	servicePathLabel, hasServicePath := labels[prefix+"path"]
	if prefix == "" {
		servicePathLabel, hasServicePath = labels[apptypes.LabelServicePath]
	}
	servicePath, pathErr := parseServicePath(servicePathLabel, hasServicePath, serviceProtocol)
	if pathErr != nil {
		servicePath = ""
	}

	return &apptypes.ContainerService{
		ContainerID:     id,
		ContainerName:   containerName,
		ServiceEnabled:  true,
		ServiceName:     serviceName,
		Port:            servicePort,
		TargetPort:      targetPort,
		ServiceProtocol: serviceProtocol,
		ServicePath:     servicePath,
		Protocol:        protocol,
		Tags:            tags,
	}
}

func (c *Client) cloudTags(labels map[string]string, containerName string) []string {
	if tagsStr := labels[apptypes.LabelTags]; tagsStr != "" {
		var tags []string
		for _, part := range strings.Split(tagsStr, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				if !strings.HasPrefix(trimmed, "tag:") {
					log.Warn().
						Str("container", containerName).
						Str("tag", trimmed).
						Msg("Tag should start with 'tag:' prefix per Tailscale convention")
				}
				tags = append(tags, trimmed)
			}
		}
		return tags
	}
	tags := make([]string, len(c.defaultTags))
	copy(tags, c.defaultTags)
	return tags
}

func indexedServiceNumbers(labels map[string]string) []int {
	indices := map[int]struct{}{}
	for key := range labels {
		matches := indexedPortRegex.FindStringSubmatch(key)
		if matches == nil {
			continue
		}
		idx, err := strconv.Atoi(matches[1])
		if err != nil {
			continue
		}
		indices[idx] = struct{}{}
	}
	sorted := make([]int, 0, len(indices))
	for idx := range indices {
		sorted = append(sorted, idx)
	}
	sort.Ints(sorted)
	return sorted
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// EngineID returns the docker engine ID — the stable host fingerprint and the
// cloud's billable unit. Persisted by dockerd; survives reboots/reinstalls.
func (c *Client) EngineID(ctx context.Context) (string, error) {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	if info.ID == "" {
		return "", fmt.Errorf("docker engine ID is empty")
	}
	return info.ID, nil
}

// ServerVersion returns the docker server version string (best-effort).
func (c *Client) ServerVersion(ctx context.Context) string {
	v, err := c.cli.ServerVersion(ctx)
	if err != nil {
		return ""
	}
	return v.Version
}

// HostSpec is static host capacity read once from `docker info` (display-only).
type HostSpec struct {
	OS            string
	KernelVersion string
	Arch          string
	CPUCores      int
	MemTotalBytes int64
}

// HostSpecs reads static host capacity from `docker info` (best-effort; zero
// values on error). Read once at agent start — these do not change at runtime.
func (c *Client) HostSpecs(ctx context.Context) HostSpec {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return HostSpec{}
	}
	return HostSpec{
		OS:            info.OperatingSystem,
		KernelVersion: info.KernelVersion,
		Arch:          info.Architecture,
		CPUCores:      info.NCPU,
		MemTotalBytes: info.MemTotal,
	}
}

// Hostname returns the host's name as docker reports it (display-only).
func (c *Client) Hostname(ctx context.Context) string {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return ""
	}
	return info.Name
}

// RestartCount best-effort reads a container's live restart count via inspect.
func (c *Client) RestartCount(ctx context.Context, containerID string) int {
	if containerID == "" {
		return 0
	}
	in, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return 0
	}
	return in.RestartCount
}

// InspectCloud inspects a container and extracts the runtime fields the cloud
// catalog wants. It is read-only.
func (c *Client) InspectCloud(ctx context.Context, containerID string) (CloudInfo, error) {
	in, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return CloudInfo{}, fmt.Errorf("inspect container: %w", err)
	}

	var info CloudInfo
	image := in.Image
	var labels map[string]string
	if in.Config != nil {
		labels = in.Config.Labels
		if in.Config.Image != "" {
			image = in.Config.Image
		}
	}
	info.Image, info.ImageTag = splitImageTag(image)
	info.RestartCount = in.RestartCount

	if labels != nil {
		info.ComposeProject = labels["com.docker.compose.project"]
		info.ComposeService = labels["com.docker.compose.service"]
	}

	if in.State != nil {
		info.State = in.State.Status
		if in.State.Health != nil {
			info.Health = in.State.Health.Status
		}
	}

	if in.NetworkSettings != nil {
		for name := range in.NetworkSettings.Networks {
			info.Networks = append(info.Networks, name)
		}
	}

	return info, nil
}

// ContainerStatsSample is a single one-shot resource reading for a container.
// CPU is reported as the raw cumulative counters docker exposes; a percentage
// is the delta between two samples, so the caller keeps the previous one.
// Memory is the cache-adjusted working set and its effective limit, both ready
// to use directly.
type ContainerStatsSample struct {
	CPUTotalUsage  uint64 // cumulative container CPU time (ns)
	CPUSystemUsage uint64 // cumulative host CPU time (ns)
	OnlineCPUs     uint64 // CPUs available, for percentage normalization
	MemUsageBytes  int64  // working set: usage minus inactive file cache
	MemLimitBytes  int64  // effective limit: container limit, else host total
}

// ContainerStats reads a single (non-streaming) docker stats sample for a
// container via the one-shot stats endpoint. It is read-only and used only by
// the optional cloud module. PreCPUStats is intentionally ignored — it is zero
// for a one-shot read, so CPU percentages are computed by the caller across
// successive samples.
func (c *Client) ContainerStats(ctx context.Context, containerID string) (ContainerStatsSample, error) {
	resp, err := c.cli.ContainerStatsOneShot(ctx, containerID)
	if err != nil {
		return ContainerStatsSample{}, fmt.Errorf("container stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var v container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return ContainerStatsSample{}, fmt.Errorf("decode stats: %w", err)
	}

	onlineCPUs := uint64(v.CPUStats.OnlineCPUs)
	if onlineCPUs == 0 {
		onlineCPUs = uint64(len(v.CPUStats.CPUUsage.PercpuUsage))
	}
	return ContainerStatsSample{
		CPUTotalUsage:  v.CPUStats.CPUUsage.TotalUsage,
		CPUSystemUsage: v.CPUStats.SystemUsage,
		OnlineCPUs:     onlineCPUs,
		MemUsageBytes:  memUsageNoCache(v.MemoryStats),
		MemLimitBytes:  int64(v.MemoryStats.Limit),
	}, nil
}

// memUsageNoCache returns the container's working set — total memory usage minus
// the inactive file cache — matching what `docker stats` shows. The cache key
// differs between cgroup v1 (total_inactive_file) and v2 (inactive_file); falls
// back to the raw usage when neither is present.
func memUsageNoCache(mem container.MemoryStats) int64 {
	if v, ok := mem.Stats["total_inactive_file"]; ok && v < mem.Usage { // cgroup v1
		return int64(mem.Usage - v)
	}
	if v, ok := mem.Stats["inactive_file"]; ok && v < mem.Usage { // cgroup v2
		return int64(mem.Usage - v)
	}
	return int64(mem.Usage)
}

// ContainerLogsTail returns the last n log lines of a container plus the total
// captured byte size, reading both stdout and stderr.
func (c *Client) ContainerLogsTail(ctx context.Context, containerID string, n int) ([]string, int, error) {
	rc, err := c.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(n),
		Timestamps: false,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("container logs: %w", err)
	}
	defer func() { _ = rc.Close() }()

	var lines []string
	total := 0
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		s := string(stripLogHeader(scanner.Bytes()))
		lines = append(lines, s)
		total += len(s) + 1
	}
	if err := scanner.Err(); err != nil {
		return lines, total, fmt.Errorf("read logs: %w", err)
	}
	if n > 0 && len(lines) > n {
		drop := len(lines) - n
		for _, d := range lines[:drop] {
			total -= len(d) + 1
		}
		lines = lines[drop:]
	}
	if total < 0 {
		total = 0
	}
	return lines, total, nil
}

// splitImageTag splits "repo:tag" into ("repo", "tag"), registry-aware: a colon
// in a registry host:port (which has a "/" after it) is not a tag.
func splitImageTag(image string) (repo, tag string) {
	if image == "" {
		return "", ""
	}
	idx := strings.LastIndex(image, ":")
	if idx < 0 {
		return image, ""
	}
	if strings.Contains(image[idx:], "/") {
		return image, ""
	}
	return image[:idx], image[idx+1:]
}

// stripLogHeader removes the 8-byte multiplexing header docker prepends to each
// log frame when the container has no TTY. Returned unchanged for TTY containers.
func stripLogHeader(b []byte) []byte {
	if len(b) >= 8 && b[0] <= 2 && b[1] == 0 && b[2] == 0 && b[3] == 0 {
		return bytes.TrimRight(b[8:], "\r")
	}
	return bytes.TrimRight(b, "\r")
}
