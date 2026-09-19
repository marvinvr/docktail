//go:build !linux

package cloud

import "github.com/marvinvr/docktail/cloud/proto"

// Disk usage is read through statfs(2) over /proc/mounts, neither of which is
// portable, so everywhere but Linux the agent reports no filesystems and never
// advertises proto.CapHostDisk. The rest of the host vitals are unaffected.

func readFilesystems() []proto.Filesystem { return nil }

func hostFSAvailable() bool { return false }
