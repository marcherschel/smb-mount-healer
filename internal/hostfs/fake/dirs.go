package fake

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Dir is the fake content of a local (non-mounted) directory: only the
// aggregates a quarantine would carry along.
type Dir struct {
	Entries int // top-level entries; 0 means empty
	Files   int64
	Bytes   int64
	ModTime time.Time
	NotDir  bool // the path exists but is a file
}

// Dirs implements hostfs.DirInspector over a map of paths. Pod volume dirs
// and globalmount dirs are derived from the paths that match Layout.
type Dirs struct {
	Layout kubelet.Layout

	mounts *MountTable
	mu     sync.Mutex
	dirs   map[string]Dir
	errs   map[string]error
}

// SetDir creates or replaces the directory at path.
func (d *Dirs) SetDir(path string, dir Dir) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dirs == nil {
		d.dirs = map[string]Dir{}
	}
	d.dirs[filepath.Clean(path)] = dir
}

// RemoveDir removes path (like kubelet's os.Remove after unstage/unpublish).
// It refuses (false) when the dir is not empty or is a mountpoint.
func (d *Dirs) RemoveDir(path string) bool {
	path = filepath.Clean(path)
	if d.mounts.IsMountpoint(path) {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dir, ok := d.dirs[path]
	if !ok || dir.Entries > 0 {
		return false
	}
	delete(d.dirs, path)
	return true
}

// Dir returns the directory at path.
func (d *Dirs) Dir(path string) (Dir, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dir, ok := d.dirs[filepath.Clean(path)]
	return dir, ok
}

// SetError makes Inspect(path) fail with err (nil clears it).
func (d *Dirs) SetError(path string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.errs == nil {
		d.errs = map[string]error{}
	}
	if err == nil {
		delete(d.errs, path)
		return
	}
	d.errs[path] = err
}

// Inspect implements hostfs.DirInspector.
func (d *Dirs) Inspect(_ context.Context, path string) (hostfs.DirFacts, error) {
	path = filepath.Clean(path)
	if d.mounts.IsMountpoint(path) {
		return hostfs.DirFacts{}, fmt.Errorf("inspect %s: %w", path, hostfs.ErrIsMountpoint)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.errs[path]; err != nil {
		return hostfs.DirFacts{}, err
	}
	dir, ok := d.dirs[path]
	if !ok {
		return hostfs.DirFacts{}, nil
	}
	if dir.NotDir {
		return hostfs.DirFacts{Exists: true}, nil
	}
	return hostfs.DirFacts{Exists: true, IsDir: true, Empty: dir.Entries == 0}, nil
}

// ListPodVolumeDirs implements hostfs.DirInspector.
func (d *Dirs) ListPodVolumeDirs(_ context.Context) ([]hostfs.PodVolumeDir, error) {
	prefix := d.Layout.PodsRoot() + "/"
	var out []hostfs.PodVolumeDir
	for _, p := range d.paths() {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		parts := strings.Split(rest, "/") // <uid>/volumes/kubernetes.io~csi/<pv>/mount
		if len(parts) == 5 && parts[1] == "volumes" && parts[2] == "kubernetes.io~csi" && parts[4] == "mount" {
			out = append(out, hostfs.PodVolumeDir{PodUID: types.UID(parts[0]), PVName: parts[3], Path: p})
		}
	}
	return out, nil
}

// ListGlobalmountDirs implements hostfs.DirInspector.
func (d *Dirs) ListGlobalmountDirs(_ context.Context) ([]hostfs.GlobalmountDir, error) {
	prefix := d.Layout.StagingRoot() + "/"
	var out []hostfs.GlobalmountDir
	for _, p := range d.paths() {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok {
			continue
		}
		if hash, ok := strings.CutSuffix(rest, "/globalmount"); ok && !strings.Contains(hash, "/") {
			out = append(out, hostfs.GlobalmountDir{Hash: hash, Path: p})
		}
	}
	return out, nil
}

func (d *Dirs) paths() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.dirs))
	for p := range d.dirs {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// take empties path and returns what it held.
func (d *Dirs) take(path string) (Dir, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dir, ok := d.dirs[path]
	if !ok || dir.NotDir || dir.Entries == 0 {
		return Dir{}, false
	}
	d.dirs[path] = Dir{ModTime: dir.ModTime}
	return dir, true
}
