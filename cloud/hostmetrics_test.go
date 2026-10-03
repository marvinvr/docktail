package cloud

import (
	"os"
	"path/filepath"
	"testing"
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
