package cloud

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/marvinvr/docktail/cloud/proto"
)

// Host-vitals sources. In a normal Linux container the system-wide /proc files
// are the HOST's — they are not namespaced (that is exactly what LXCFS exists to
// fake), so the agent reads true whole-host CPU/memory/load with no extra
// mounts. On Docker Desktop these reflect the LinuxKit VM; /sys temperature
// sensors exist only on bare metal. Every read is best-effort: a missing or
// unreadable source yields "unknown", never an error that stops reporting.
// Package vars (not consts) so they can be redirected in tests.
var (
	procDir = "/proc"
	sysDir  = "/sys"
)

// maxTempReadings caps the per-zone detail we report; the max is the glance
// value, the list is supporting detail.
const maxTempReadings = 12

// hostMetricsReader samples whole-host utilization from /proc and /sys. CPU% is
// a delta of cumulative jiffies between successive samples, so it carries the
// previous /proc/stat reading across calls (and across reconnects, since the
// reader lives on the Collector). sample() is guarded by a mutex because two
// per-connection metrics loops can briefly overlap during a reconnect.
type hostMetricsReader struct {
	mu        sync.Mutex
	prev      cpuJiffies
	havePrev  bool
	readTemps bool // whether temperature sensors were detected at startup
	readDisk  bool // whether any filesystem was enumerable at startup
}

type cpuJiffies struct {
	total uint64
	idle  uint64
}

// newHostMetricsReader builds a reader and probes once whether temperature
// sensors and filesystems are readable, so the agent can advertise matching
// capabilities.
func newHostMetricsReader() *hostMetricsReader {
	r := &hostMetricsReader{}
	if _, list := readTemps(); len(list) > 0 {
		r.readTemps = true
	}
	r.readDisk = hostFSAvailable()
	return r
}

// available reports whether core host vitals (CPU + memory) are readable here.
// If not (non-Linux, or a restricted /proc), the agent does not advertise
// host_metrics and never starts the metrics loop.
func (r *hostMetricsReader) available() bool {
	if _, ok := readCPUJiffies(); !ok {
		return false
	}
	return readMemInfo().totalBytes > 0
}

// tempAvailable reports whether at least one temperature sensor was found.
func (r *hostMetricsReader) tempAvailable() bool { return r.readTemps }

// diskAvailable reports whether at least one filesystem could be enumerated.
func (r *hostMetricsReader) diskAvailable() bool { return r.readDisk }

// sample reads one set of host vitals. CPU% is nil until a second sample
// establishes a delta. Temperature and disk are read only when sensors /
// filesystems were detected at startup. Fields the host can't supply are left
// zero/nil and the cloud stores them as NULL.
func (r *hostMetricsReader) sample() proto.HostMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()

	var m proto.HostMetrics

	if cur, ok := readCPUJiffies(); ok {
		if r.havePrev {
			if pct, ok := cpuPercent(r.prev, cur); ok {
				m.CPUPercent = &pct
			}
		}
		r.prev = cur
		r.havePrev = true
	}

	if mem := readMemInfo(); mem.totalBytes > 0 {
		m.MemTotalBytes = mem.totalBytes
		m.MemUsedBytes = mem.usedBytes
		m.SwapTotalBytes = mem.swapTotalBytes
		m.SwapUsedBytes = mem.swapUsedBytes
	}

	if l1, l5, l15, ok := readLoadAvg(); ok {
		m.Load1, m.Load5, m.Load15 = &l1, &l5, &l15
	}

	if r.readTemps {
		if max, list := readTemps(); len(list) > 0 {
			m.TempMaxC, m.Temps = max, list
		}
	}

	if r.readDisk {
		m.Filesystems = readFilesystems()
	}

	return m
}

// ---- /proc/stat (CPU) -----------------------------------------------------

// readCPUJiffies reads the aggregate "cpu" line of /proc/stat. total is the sum
// of all time fields; idle is idle+iowait. Both are cumulative since boot.
func readCPUJiffies() (cpuJiffies, bool) {
	f, err := os.Open(filepath.Join(procDir, "stat"))
	if err != nil {
		return cpuJiffies{}, false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop the "cpu" label
		var total, idle uint64
		for i, fld := range fields {
			// Fields are user nice system idle iowait irq softirq steal guest
			// guest_nice. guest (8) is already counted within user (0) and
			// guest_nice (9) within nice (1) by kernel accounting, so summing them
			// would double-count — stop at the first 8.
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(fld, 10, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		if total == 0 {
			return cpuJiffies{}, false
		}
		return cpuJiffies{total: total, idle: idle}, true
	}
	return cpuJiffies{}, false
}

// physicalCPUCount counts the per-core "cpuN" lines in /proc/stat — the number of
// logical CPUs the kernel that owns this /proc has. Inside a Proxmox LXC the
// agent's /proc is the physical NODE's, so this is the node's core count, which
// exceeds the CT's docker-reported cores; the collector uses that gap to tell
// node-scoped loadavg from the container's. Returns 0 on any read error.
func physicalCPUCount() int {
	f, err := os.Open(filepath.Join(procDir, "stat"))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if len(line) > 3 && line[:3] == "cpu" && line[3] >= '0' && line[3] <= '9' {
			n++
		}
	}
	return n
}

// cpuPercent computes utilization between two cumulative readings, clamped to
// 0..100. ok is false when the counter did not advance (or went backwards).
func cpuPercent(prev, cur cpuJiffies) (float64, bool) {
	if cur.total <= prev.total {
		return 0, false
	}
	totalDelta := float64(cur.total - prev.total)
	var idleDelta float64
	if cur.idle >= prev.idle {
		idleDelta = float64(cur.idle - prev.idle)
	}
	busy := totalDelta - idleDelta
	if busy < 0 {
		busy = 0
	}
	pct := math.Round(busy/totalDelta*100*100) / 100 // 2 decimals
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return pct, true
}

// ---- /proc/meminfo (memory + swap) ----------------------------------------

type memInfo struct {
	totalBytes     int64
	usedBytes      int64
	swapTotalBytes int64
	swapUsedBytes  int64
}

// readMemInfo parses /proc/meminfo. Used is MemTotal-MemAvailable (the kernel's
// own pressure estimate, which discounts reclaimable page cache), falling back
// to free+buffers+cached on kernels too old for MemAvailable. Swap used is
// SwapTotal-SwapFree. meminfo reports kibibytes.
func readMemInfo() memInfo {
	f, err := os.Open(filepath.Join(procDir, "meminfo"))
	if err != nil {
		return memInfo{}
	}
	defer func() { _ = f.Close() }()

	vals := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if key, kb, ok := parseMeminfoLine(sc.Text()); ok {
			vals[key] = kb
		}
	}

	const kib = 1024
	total := vals["MemTotal"]
	if total <= 0 {
		return memInfo{}
	}
	var used int64
	if avail, ok := vals["MemAvailable"]; ok {
		used = total - avail
	} else {
		used = total - (vals["MemFree"] + vals["Buffers"] + vals["Cached"])
	}
	if used < 0 {
		used = 0
	}
	swapUsed := vals["SwapTotal"] - vals["SwapFree"]
	if swapUsed < 0 {
		swapUsed = 0
	}
	return memInfo{
		totalBytes:     total * kib,
		usedBytes:      used * kib,
		swapTotalBytes: vals["SwapTotal"] * kib,
		swapUsedBytes:  swapUsed * kib,
	}
}

// parseMeminfoLine parses a line like "MemTotal:       16331156 kB" into its key
// and kibibyte value.
func parseMeminfoLine(line string) (key string, kb int64, ok bool) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 {
		return "", 0, false
	}
	fields := strings.Fields(line[colon+1:])
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return line[:colon], v, true
}

// ---- /proc/loadavg (load) -------------------------------------------------

// readLoadAvg parses /proc/loadavg: "0.52 0.58 0.59 1/843 12345".
func readLoadAvg() (l1, l5, l15 float64, ok bool) {
	b, err := os.ReadFile(filepath.Join(procDir, "loadavg"))
	if err != nil {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return 0, 0, 0, false
	}
	var e1, e5, e15 error
	l1, e1 = strconv.ParseFloat(fields[0], 64)
	l5, e5 = strconv.ParseFloat(fields[1], 64)
	l15, e15 = strconv.ParseFloat(fields[2], 64)
	if e1 != nil || e5 != nil || e15 != nil {
		return 0, 0, 0, false
	}
	return l1, l5, l15, true
}

// ---- /sys temperature sensors ---------------------------------------------

// plausibleTempC bounds a reading to a sane range, so a bogus 0 or a
// raw-millidegree misread never pollutes the max.
func plausibleTempC(c float64) bool { return c > 0 && c < 150 }

// readTemps reads temperature sensors from /sys/class/thermal (thermal_zone*)
// and /sys/class/hwmon (hwmon*/temp*_input). It returns the hottest reading and
// a labeled per-zone list (both nil/empty when no sensors are present, as on VMs
// and Docker Desktop). Best-effort: unreadable or implausible entries skipped.
func readTemps() (*float64, []proto.TempReading) {
	out := append(readThermalZones(), readHwmonTemps()...)
	if len(out) == 0 {
		return nil, nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Celsius > out[j].Celsius })
	max := out[0].Celsius
	if len(out) > maxTempReadings {
		out = out[:maxTempReadings]
	}
	return &max, out
}

func readThermalZones() []proto.TempReading {
	dirs, _ := filepath.Glob(filepath.Join(sysDir, "class", "thermal", "thermal_zone*"))
	var out []proto.TempReading
	for _, d := range dirs {
		c, ok := readMilliCelsius(filepath.Join(d, "temp"))
		if !ok {
			continue
		}
		label := filepath.Base(d)
		if t, err := os.ReadFile(filepath.Join(d, "type")); err == nil {
			if s := strings.TrimSpace(string(t)); s != "" {
				label = s
			}
		}
		out = append(out, proto.TempReading{Label: label, Celsius: c})
	}
	return out
}

func readHwmonTemps() []proto.TempReading {
	inputs, _ := filepath.Glob(filepath.Join(sysDir, "class", "hwmon", "hwmon*", "temp*_input"))
	var out []proto.TempReading
	for _, in := range inputs {
		c, ok := readMilliCelsius(in)
		if !ok {
			continue
		}
		out = append(out, proto.TempReading{Label: hwmonLabel(in), Celsius: c})
	}
	return out
}

// readMilliCelsius reads a sysfs millidegree-Celsius file and returns degrees C
// rounded to one decimal, dropping implausible values.
func readMilliCelsius(path string) (float64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	milli, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	c := math.Round(float64(milli)/1000*10) / 10
	if !plausibleTempC(c) {
		return 0, false
	}
	return c, true
}

// hwmonLabel builds a readable label for a hwmonN/tempX_input file from its
// sibling tempX_label (preferred) and the chip's name file, e.g.
// "coretemp:Package id 0".
func hwmonLabel(inputPath string) string {
	dir := filepath.Dir(inputPath)
	prefix := strings.TrimSuffix(filepath.Base(inputPath), "_input") // tempX
	var chip, label string
	if n, err := os.ReadFile(filepath.Join(dir, "name")); err == nil {
		chip = strings.TrimSpace(string(n))
	}
	if l, err := os.ReadFile(filepath.Join(dir, prefix+"_label")); err == nil {
		label = strings.TrimSpace(string(l))
	}
	switch {
	case chip != "" && label != "":
		return chip + ":" + label
	case label != "":
		return label
	case chip != "":
		return chip + ":" + prefix
	default:
		return prefix
	}
}
