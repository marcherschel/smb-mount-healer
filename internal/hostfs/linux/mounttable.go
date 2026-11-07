package linux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/moby/sys/mountinfo"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// DefaultMountInfoPath is the mount table of the worker's own mount
// namespace. With /var/lib/kubelet mounted Bidirectional it shows the host's
// mounts under the kubelet root.
const DefaultMountInfoPath = "/proc/self/mountinfo"

// MountTable reads mountinfo, keeping only mountpoints at or below Prefix.
type MountTable struct {
	// Prefix is an absolute, cleaned path (e.g. KUBELET_ROOT). "/" or ""
	// keeps every entry.
	Prefix string
	// Path is the mountinfo file; DefaultMountInfoPath if empty.
	Path string
}

var _ hostfs.MountTable = MountTable{}

// NewMountTable returns a MountTable for /proc/self/mountinfo filtered on prefix.
func NewMountTable(prefix string) MountTable {
	return MountTable{Prefix: filepath.Clean(prefix)}
}

// Snapshot reads the mount table once. Entries keep mountinfo order, so
// MountSnapshot.Lookup returns the visible (last) mount of an overmount.
func (t MountTable) Snapshot(_ context.Context) (hostfs.MountSnapshot, error) {
	path := t.Path
	if path == "" {
		path = DefaultMountInfoPath
	}
	f, err := os.Open(path) //nolint:gosec // fixed or configured mountinfo path
	if err != nil {
		return hostfs.MountSnapshot{}, fmt.Errorf("read mountinfo: %w", err)
	}
	defer func() { _ = f.Close() }()

	var filter mountinfo.FilterFunc
	if p := filepath.Clean(t.Prefix); t.Prefix != "" && p != "/" {
		filter = mountinfo.PrefixFilter(p)
	}
	infos, err := mountinfo.GetMountsFromReader(f, filter)
	if err != nil {
		return hostfs.MountSnapshot{}, fmt.Errorf("parse mountinfo: %w", err)
	}
	snap := hostfs.MountSnapshot{Entries: make([]hostfs.MountEntry, 0, len(infos))}
	for _, in := range infos {
		snap.Entries = append(snap.Entries, hostfs.MountEntry{
			ID:           in.ID,
			ParentID:     in.Parent,
			MountPoint:   in.Mountpoint,
			Root:         in.Root,
			FSType:       in.FSType,
			Source:       in.Source,
			Options:      in.Options,
			SuperOptions: in.VFSOptions,
		})
	}
	return snap, nil
}

// mountedAtOrBelow reports whether path or anything below it is a mountpoint.
func mountedAtOrBelow(snap hostfs.MountSnapshot, path string) bool {
	for _, e := range snap.Entries {
		if isSubpath(e.MountPoint, path) {
			return true
		}
	}
	return false
}

// isSubpath reports whether p equals base or lies below it. Both are cleaned.
func isSubpath(p, base string) bool {
	if p == base {
		return true
	}
	if base == "/" {
		return len(p) > 1 && p[0] == '/'
	}
	return len(p) > len(base) && p[len(base)] == '/' && p[:len(base)] == base
}

// strictlyBelow reports whether p lies strictly below base.
func strictlyBelow(p, base string) bool { return p != base && isSubpath(p, base) }
