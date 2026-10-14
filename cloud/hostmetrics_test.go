package cloud

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marvinvr/docktail/cloud/proto"
)

func writeProcFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func useProcDir(t *testing.T, dir string) {
	t.Helper()
	prev := procDir
	procDir = dir
	t.Cleanup(func() { procDir = prev })
}

const testMeminfo = `MemTotal:       16000000 kB
MemFree:         2000000 kB
MemAvailable:    4000000 kB
SwapTotal:             0 kB
SwapFree:              0 kB
`

func TestReadMemInfoWithoutZFS(t *testing.T) {
	dir := t.TempDir()
	writeProcFile(t, dir, "meminfo", testMeminfo)
	useProcDir(t, dir)

	if got, want := readMemInfo().usedBytes, int64(12000000*1024); got != want {
		t.Fatalf("used = %d, want %d", got, want)
	}
}

func TestReadMemInfoDiscountsShrinkableARC(t *testing.T) {
	dir := t.TempDir()
	writeProcFile(t, dir, "meminfo", testMeminfo)
	writeProcFile(t, dir, "spl/kstat/zfs/arcstats", `13 1 0x01 123 33456 1234 5678
name                            type data
hits                            4    123456
size                            4    6144000000
c_min                           4    1024000000
c_max                           4    8192000000
`)
	useProcDir(t, dir)

	// 12,000,000 kB used minus (6,144,000,000 - 1,024,000,000) B / 1024 = 5,000,000 kB.
	if got, want := readMemInfo().usedBytes, int64(7000000*1024); got != want {
		t.Fatalf("used = %d, want %d", got, want)
	}
}

func TestZFSARCShrinkableAtFloor(t *testing.T) {
	dir := t.TempDir()
	writeProcFile(t, dir, "spl/kstat/zfs/arcstats", `name type data
size  4  1000
c_min 4  2000
`)
	useProcDir(t, dir)

	if got := zfsARCShrinkableBytes(); got != 0 {
		t.Fatalf("shrinkable = %d, want 0 when the ARC is at its floor", got)
	}
}

// Values seen on a Proxmox LXC with a 12 GiB limit on a 32 GB node: the agent's
// /proc/meminfo is the node's, docker info reports the CT's limit via LXCFS.
const nodeMeminfo = `MemTotal:       32631304 kB
MemFree:         8018464 kB
MemAvailable:   18320868 kB
SwapTotal:       8388604 kB
SwapFree:        8000000 kB
`

func TestMemNodeScoped(t *testing.T) {
	dir := t.TempDir()
	writeProcFile(t, dir, "meminfo", nodeMeminfo)
	useProcDir(t, dir)

	const procTotal = int64(32631304 * 1024)
	cases := []struct {
		name        string
		dockerTotal int64
		want        bool
	}{
		{"lxc limit below node", 12 << 30, true},
		{"same view (bare metal, VM, Docker Desktop)", procTotal, false},
		{"within margin", procTotal - procTotal/100, false},
		{"docker info unreadable", 0, false},
	}
	for _, tc := range cases {
		if got := memNodeScoped(tc.dockerTotal); got != tc.want {
			t.Errorf("%s: memNodeScoped(%d) = %v, want %v", tc.name, tc.dockerTotal, got, tc.want)
		}
	}
}

func TestMemNodeScopedWithoutMeminfo(t *testing.T) {
	useProcDir(t, t.TempDir())
	if memNodeScoped(12 << 30) {
		t.Fatal("unreadable /proc/meminfo must not be treated as node-scoped")
	}
}

func TestCTScopedMemoryReplacesNodeFigures(t *testing.T) {
	dir := t.TempDir()
	writeProcFile(t, dir, "meminfo", nodeMeminfo)
	useProcDir(t, dir)

	m := (&hostMetricsReader{}).sample()
	if m.MemTotalBytes != 32631304*1024 || m.SwapTotalBytes == 0 {
		t.Fatalf("precondition: node sample = %+v", m)
	}
	ctScopedMemory(&m, 12<<30, 3058742329, true)
	if m.MemTotalBytes != 12<<30 || m.MemUsedBytes != 3058742329 {
		t.Fatalf("mem = %d/%d, want 3058742329/%d", m.MemUsedBytes, m.MemTotalBytes, int64(12<<30))
	}
	if m.SwapTotalBytes != 0 || m.SwapUsedBytes != 0 {
		t.Fatalf("node swap leaked into CT report: %d/%d", m.SwapUsedBytes, m.SwapTotalBytes)
	}
}

func TestCTScopedMemoryUnreadableIsUnknown(t *testing.T) {
	m := proto.HostMetrics{MemTotalBytes: 32 << 30, MemUsedBytes: 14 << 30, SwapTotalBytes: 8 << 30, SwapUsedBytes: 1 << 30}
	ctScopedMemory(&m, 12<<30, 0, false)
	if m.MemTotalBytes != 0 || m.MemUsedBytes != 0 || m.SwapTotalBytes != 0 || m.SwapUsedBytes != 0 {
		t.Fatalf("expected unknown memory, got %+v", m)
	}
}
