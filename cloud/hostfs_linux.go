//go:build linux

package cloud

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/marvinvr/docktail/cloud/proto"
)

// EnvHostRoot names a read-only bind mount of the HOST's filesystem root
// (`- /:/host:ro`), which is what makes the host's other filesystems visible
// from inside the agent container. It defaults to /host when that path holds a
// readable host mount table and is otherwise empty — the zero-configuration
// case, in which the agent reports only the filesystem backing its own root.
const EnvHostRoot = "DOCKTAIL_HOST_ROOT"

const defaultHostRoot = "/host"

// hostRootDir is the prefix the host's own filesystem tree is visible under, or
// "" when the agent can only see its own mount namespace. Resolved once (a
// bind mount does not appear mid-run) and a package var so tests can redirect
// it, exactly like procDir/sysDir.
var hostRootDir = resolveHostRoot()

// allowedFSTypes is an ALLOW-list, not a deny-list, and that is the whole point:
// it drops every pseudo filesystem (proc, sysfs, tmpfs, cgroup2, devpts, …), the
// dozens of per-container overlay/shm mounts under /var/lib/docker, and — most
// importantly — network filesystems. statfs(2) on a hung NFS or CIFS mount
// blocks indefinitely, and sampling runs on the heartbeat ticker with no
// timeout, so one dead NAS would stop host vitals entirely. Anything new and
// unrecognized is simply not reported, which is the safe direction.
var allowedFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "jfs": true, "bcachefs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
}

// skippedMounts are mount points (and everything below them) never worth
// reporting even when the filesystem type passes: kernel and runtime trees, and
// the per-container/per-pod mount farms that would otherwise report the same
// device dozens of times.
var skippedMounts = []string{
	"/proc", "/sys", "/dev", "/run",
	"/var/lib/docker", "/var/lib/kubelet", "/snap",
}

// readFilesystems reports per-mount disk usage for the real filesystems this
// agent can see, busiest first and capped at proto.MaxFilesystems.
//
// Without a host bind mount the agent's own mount namespace shows `overlay` on
// /, and statfs passes through to the upper layer — so that one reading is the
// real filesystem backing /var/lib/docker, usually the host root. With
// `- /:/host:ro` the host's init mount table is readable and every host
// filesystem is reported instead. Best-effort throughout: an unreadable mount
// table or a failing statfs yields fewer entries, never an error.
func readFilesystems() []proto.Filesystem {
	points := readMountTable()
	if len(points) == 0 {
		return nil
	}

	// Shortest path first, so a bind mount of a filesystem already reported
	// loses to the place it is actually mounted.
	sort.Slice(points, func(i, j int) bool {
		if len(points[i]) != len(points[j]) {
			return len(points[i]) < len(points[j])
		}
		return points[i] < points[j]
	})

	seen := make(map[uint64]bool, len(points))
	out := make([]proto.Filesystem, 0, len(points))
	for _, point := range points {
		// Report the un-prefixed mount point: the cloud must show /mnt/data,
		// never this container's /host/mnt/data.
		path := filepath.Join(hostRootDir, point)
		dev, ok := deviceOf(path)
		if !ok || seen[dev] {
			continue
		}
		fs, ok := statfsBytes(path)
		if !ok {
			continue
		}
		seen[dev] = true
		fs.Mount = point
		out = append(out, fs)
	}
	if len(out) == 0 {
		return nil
	}

	sort.Slice(out, func(i, j int) bool { return usedFraction(out[i]) > usedFraction(out[j]) })
	if len(out) > proto.MaxFilesystems {
		out = out[:proto.MaxFilesystems]
	}
	return out
}

// hostFSAvailable reports whether any filesystem is readable here, so the agent
// advertises proto.CapHostDisk only when it can actually answer.
func hostFSAvailable() bool { return len(readFilesystems()) > 0 }

// resolveHostRoot picks the prefix the host's filesystem is visible under: the
// configured or default bind mount when it exposes the host's init mount table,
// else "" (report the agent's own view). The mount table decides, not the mere
// existence of the directory, so a stray /host never silently redirects the read.
func resolveHostRoot() string {
	root := defaultHostRoot
	if v := strings.TrimSpace(os.Getenv(EnvHostRoot)); v != "" {
		root = filepath.Clean(v)
	}
	if root == "" || root == "/" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "proc", "1", "mounts")); err != nil {
		return ""
	}
	return root
}

// mountsPath is the mount table to parse: PID 1's under a host bind mount (the
// host's own, since docker bind mounts are recursive and /host/proc is the host
// procfs), else this process's.
func mountsPath() string {
	if hostRootDir != "" {
		return filepath.Join(hostRootDir, "proc", "1", "mounts")
	}
	return filepath.Join(procDir, "mounts")
}

// readMountTable parses /proc/mounts ("device point fstype options dump pass")
// and returns the mount points worth a statfs.
func readMountTable() []string {
	f, err := os.Open(mountsPath())
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		point, fsType := unescapeMountField(fields[1]), fields[2]
		if !strings.HasPrefix(point, "/") || skippedMount(point) {
			continue
		}
		// overlay is allowed on / only: that is the containerized agent's own
		// root, whose statfs passes through to the filesystem backing
		// /var/lib/docker. Every other overlay mount is some other container's.
		if !allowedFSTypes[fsType] && (fsType != "overlay" || point != "/") {
			continue
		}
		out = append(out, point)
	}
	return out
}

func skippedMount(point string) bool {
	for _, p := range skippedMounts {
		if point == p || strings.HasPrefix(point, p+"/") {
			return true
		}
	}
	return false
}

// unescapeMountField decodes the octal escapes the kernel writes for characters
// that would otherwise split a field (space, tab, newline, backslash).
func unescapeMountField(value string) string {
	if !strings.Contains(value, `\`) {
		return value
	}
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

// deviceOf returns the device id backing a mount point, used to collapse bind
// mounts of one filesystem into a single reported entry. Non-directories are
// rejected: docker bind-mounts single FILES onto /etc/hosts, /etc/hostname and
// /etc/resolv.conf, and those pass the fstype allow-list while being nothing an
// operator would recognize as a filesystem.
func deviceOf(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// statfsBytes converts one statfs(2) reading to bytes. f_bsize is int32 on
// 32-bit architectures and int64 elsewhere, so it is widened once here. Avail
// (f_bavail) excludes the root reserve, which is what makes used/(used+avail)
// agree with `df` instead of reading a few points low.
func statfsBytes(path string) (proto.Filesystem, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return proto.Filesystem{}, false
	}
	if st.Bsize <= 0 || uint64(st.Blocks) == 0 {
		return proto.Filesystem{}, false
	}
	bs := uint64(st.Bsize)
	total := saturatingInt64(uint64(st.Blocks) * bs)
	free := saturatingInt64(uint64(st.Bfree) * bs)
	avail := saturatingInt64(uint64(st.Bavail) * bs)
	if total <= 0 {
		return proto.Filesystem{}, false
	}
	used := total - free
	if used < 0 {
		used = 0
	}
	if avail < 0 {
		avail = 0
	}
	return proto.Filesystem{TotalBytes: total, UsedBytes: used, AvailBytes: avail}, true
}

// saturatingInt64 converts a byte count to the int64 the report carries,
// capping it instead of wrapping to a negative value.
func saturatingInt64(u uint64) int64 {
	if u > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(u)
}

// usedFraction is the `df` reading — used of what a normal user can still fill.
func usedFraction(fs proto.Filesystem) float64 {
	if denom := fs.UsedBytes + fs.AvailBytes; denom > 0 {
		return float64(fs.UsedBytes) / float64(denom)
	}
	if fs.TotalBytes > 0 {
		return float64(fs.UsedBytes) / float64(fs.TotalBytes)
	}
	return 0
}
