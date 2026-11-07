package linux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// QuarantineInspector implements hostfs.QuarantineInspector for Dir
// (QUARANTINE_DIR). Scans are bounded by MaxEntries and TimeBudget, never
// follow symlinks, never descend into a mountpoint, and return aggregates
// only: no entry name is ever returned, logged or put into an error.
type QuarantineInspector struct {
	Dir        string
	MaxEntries int64         // <= 0: unlimited
	TimeBudget time.Duration // <= 0: unlimited
	// Mounts, if set, should be filtered on Dir; directories that are
	// mountpoints are counted but not descended into (SizeTruncated).
	Mounts hostfs.MountTable
	// Now defaults to time.Now (test hook for the time budget).
	Now func() time.Time
}

var _ hostfs.QuarantineInspector = QuarantineInspector{}

// ListFolders implements hostfs.QuarantineInspector: the directories
// directly under Dir, sorted by name. Non-directories and symlinks are
// ignored. A missing Dir is nil, nil.
func (q QuarantineInspector) ListFolders(_ context.Context) ([]hostfs.QuarantineFolder, error) {
	ents, err := os.ReadDir(q.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list quarantine dir %s: %w", q.Dir, sanitize("readdir", err))
	}
	var out []hostfs.QuarantineFolder
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, hostfs.QuarantineFolder{Name: e.Name(), Path: filepath.Join(q.Dir, e.Name())})
		}
	}
	return out, nil
}

// Scan implements hostfs.QuarantineInspector. folderPath must be a direct
// child of Dir. Unreadable entries inside the folder and an exhausted budget
// both set SizeTruncated; the aggregates are then lower bounds.
func (q QuarantineInspector) Scan(ctx context.Context, folderPath string) (hostfs.QuarantineStats, error) {
	folder := filepath.Clean(folderPath)
	if filepath.Dir(folder) != filepath.Clean(q.Dir) {
		return hostfs.QuarantineStats{}, fmt.Errorf("scan %s: not a folder directly under %s", folder, q.Dir)
	}
	var st unix.Stat_t
	if err := unix.Lstat(folder, &st); err != nil {
		return hostfs.QuarantineStats{}, fmt.Errorf("scan %s: %w", folder, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return hostfs.QuarantineStats{}, fmt.Errorf("scan %s: not a directory", folder)
	}
	var snap hostfs.MountSnapshot
	if q.Mounts != nil {
		var err error
		if snap, err = q.Mounts.Snapshot(ctx); err != nil {
			return hostfs.QuarantineStats{}, err
		}
		if snap.IsMountpoint(folder) {
			return hostfs.QuarantineStats{}, fmt.Errorf("scan %s: %w", folder, hostfs.ErrIsMountpoint)
		}
	}
	now := q.Now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(q.TimeBudget)

	w := walker{snap: snap, max: q.MaxEntries}
	stack := []string{folder}
	stop := false
	for len(stack) > 0 && !stop {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ents, err := readDirents(dir, 0)
		if err != nil {
			if dir == folder {
				return hostfs.QuarantineStats{}, fmt.Errorf("scan %s: %w", folder, sanitize("list", err))
			}
			w.stats.SizeTruncated = true // entry error: aggregates are partial
			continue
		}
		for _, e := range ents {
			if err := ctx.Err(); err != nil {
				return hostfs.QuarantineStats{}, err
			}
			if (w.max > 0 && w.seen >= w.max) || (q.TimeBudget > 0 && now().After(deadline)) {
				w.stats.SizeTruncated, stop = true, true
				break
			}
			if sub, ok := w.visit(dir, e, dir == folder); ok {
				stack = append(stack, sub)
			}
		}
	}
	return w.stats, nil
}

type walker struct {
	snap  hostfs.MountSnapshot
	max   int64
	seen  int64
	stats hostfs.QuarantineStats
}

// visit accounts one entry (lstat, never following symlinks) and returns the
// directory to descend into, if any.
func (w *walker) visit(dir string, e dirent, top bool) (string, bool) {
	w.seen++
	if top {
		w.stats.Entries++
	}
	p := filepath.Join(dir, e.name)
	if e.typ == unix.DT_DIR && w.snap.IsMountpoint(p) {
		w.stats.SizeTruncated = true // never walk into a mount
		return "", false
	}
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		w.stats.SizeTruncated = true
		return "", false
	}
	mt := time.Unix(st.Mtim.Unix())
	if w.stats.OldestModTime.IsZero() || mt.Before(w.stats.OldestModTime) {
		w.stats.OldestModTime = mt
	}
	if mt.After(w.stats.NewestModTime) {
		w.stats.NewestModTime = mt
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		w.stats.Files++
		w.stats.Bytes += st.Size
	case unix.S_IFDIR:
		return p, true
	}
	return "", false
}
