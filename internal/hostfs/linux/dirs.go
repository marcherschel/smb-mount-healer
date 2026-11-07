package linux

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// DirInspector reads the kubelet directory tree. Every directory it reads is
// first checked against a fresh mountinfo snapshot: a directory strictly
// below Layout.Root that is a mountpoint is never opened, stat'ed or read.
// (The root itself and its ancestors may be mountpoints, e.g. the hostPath
// bind of /var/lib/kubelet; they are local disk.)
type DirInspector struct {
	Layout kubelet.Layout
	Mounts hostfs.MountTable
}

var _ hostfs.DirInspector = DirInspector{}

// errParentMounted means a path's parent is a mountpoint below the root, so
// it cannot be read safely.
var errParentMounted = errors.New("parent directory is a mountpoint, refusing to read it")

func (d DirInspector) root() string { return filepath.Clean(d.Layout.Root) }

// readable reports whether dir may be read under snap.
func (d DirInspector) readable(snap hostfs.MountSnapshot, dir string) bool {
	return !strictlyBelow(dir, d.root()) || !snap.IsMountpoint(dir)
}

// Inspect implements hostfs.DirInspector. Existence and type come from the
// parent's raw directory entries; only a confirmed non-mountpoint directory
// is read (one entry) to decide emptiness.
func (d DirInspector) Inspect(ctx context.Context, path string) (hostfs.DirFacts, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return hostfs.DirFacts{}, fmt.Errorf("inspect %s: path must be absolute", path)
	}
	snap, err := d.Mounts.Snapshot(ctx)
	if err != nil {
		return hostfs.DirFacts{}, err
	}
	if snap.IsMountpoint(path) {
		return hostfs.DirFacts{}, fmt.Errorf("inspect %s: %w", path, hostfs.ErrIsMountpoint)
	}
	parent := filepath.Dir(path)
	if !d.readable(snap, parent) {
		return hostfs.DirFacts{}, fmt.Errorf("inspect %s: %w", path, errParentMounted)
	}
	ents, err := readDirents(parent, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
		return hostfs.DirFacts{}, nil
	}
	if err != nil {
		return hostfs.DirFacts{}, fmt.Errorf("inspect %s: read parent: %w", path, err)
	}
	base := filepath.Base(path)
	var ent *dirent
	for i := range ents {
		if ents[i].name == base {
			ent = &ents[i]
			break
		}
	}
	if ent == nil {
		return hostfs.DirFacts{}, nil
	}
	facts := hostfs.DirFacts{Exists: true}
	switch ent.typ {
	case unix.DT_DIR:
		facts.IsDir = true
	case unix.DT_UNKNOWN:
		// Filesystem without d_type: path is confirmed not a mountpoint,
		// so an lstat is local.
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return hostfs.DirFacts{}, fmt.Errorf("inspect %s: lstat: %w", path, err)
		}
		facts.IsDir = st.Mode&unix.S_IFMT == unix.S_IFDIR
	}
	if !facts.IsDir {
		return facts, nil
	}
	first, err := readDirents(path, 1)
	if err != nil {
		return hostfs.DirFacts{}, fmt.Errorf("inspect %s: %w", path, sanitize("read directory", err))
	}
	facts.Empty = len(first) == 0
	return facts, nil
}

// subdirs lists the directory entries of dir (type DT_DIR, or DT_UNKNOWN
// which is kept and filtered by the next read). Missing dir is nil, nil.
func (d DirInspector) subdirs(snap hostfs.MountSnapshot, dir string) ([]dirent, error) {
	if !d.readable(snap, dir) {
		return nil, nil
	}
	ents, err := readDirents(dir, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	out := ents[:0]
	for _, e := range ents {
		if e.typ == unix.DT_DIR || e.typ == unix.DT_UNKNOWN {
			out = append(out, e)
		}
	}
	return out, nil
}

func hasEntry(ents []dirent, name string) bool {
	for _, e := range ents {
		if e.name == name {
			return true
		}
	}
	return false
}

// ListPodVolumeDirs implements hostfs.DirInspector: every
// <root>/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount whose parent lists
// a "mount" entry. The mount dirs themselves are not read. Sorted by path.
func (d DirInspector) ListPodVolumeDirs(ctx context.Context) ([]hostfs.PodVolumeDir, error) {
	snap, err := d.Mounts.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	podsRoot := d.Layout.PodsRoot()
	uids, err := d.subdirs(snap, podsRoot)
	if err != nil {
		return nil, err
	}
	var out []hostfs.PodVolumeDir
	for _, uid := range uids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		csiDir := filepath.Join(podsRoot, uid.name, "volumes", "kubernetes.io~csi")
		pvs, err := d.subdirs(snap, csiDir)
		if err != nil {
			return nil, err
		}
		for _, pv := range pvs {
			pvDir := filepath.Join(csiDir, pv.name)
			if !d.readable(snap, pvDir) {
				continue
			}
			ents, err := readDirents(pvDir, 0)
			if err != nil {
				continue // vanished or not a directory
			}
			if hasEntry(ents, "mount") {
				out = append(out, hostfs.PodVolumeDir{
					PodUID: types.UID(uid.name),
					PVName: pv.name,
					Path:   filepath.Join(pvDir, "mount"),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ListGlobalmountDirs implements hostfs.DirInspector: every
// <staging root>/<hash>/globalmount whose parent lists a "globalmount"
// entry. Sorted by path.
func (d DirInspector) ListGlobalmountDirs(ctx context.Context) ([]hostfs.GlobalmountDir, error) {
	snap, err := d.Mounts.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	staging := d.Layout.StagingRoot()
	hashes, err := d.subdirs(snap, staging)
	if err != nil {
		return nil, err
	}
	var out []hostfs.GlobalmountDir
	for _, h := range hashes {
		hashDir := filepath.Join(staging, h.name)
		if !d.readable(snap, hashDir) {
			continue
		}
		ents, err := readDirents(hashDir, 0)
		if err != nil {
			continue
		}
		if hasEntry(ents, "globalmount") {
			out = append(out, hostfs.GlobalmountDir{Hash: h.name, Path: filepath.Join(hashDir, "globalmount")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
