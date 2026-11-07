package fake

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// Call records one Mutator call.
type Call struct {
	Op   string // "ForceUnmount" | "Quarantine"
	Path string
	Dest string
}

// Mutator implements hostfs.Mutator over the shared fake state.
type Mutator struct {
	mounts     *MountTable
	dirs       *Dirs
	quarantine *QuarantineInspector

	mu    sync.Mutex
	errs  map[string]error
	calls []Call
}

// SetError makes ForceUnmount/Quarantine of path fail with err (nil clears it).
func (m *Mutator) SetError(path string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.errs == nil {
		m.errs = map[string]error{}
	}
	if err == nil {
		delete(m.errs, path)
		return
	}
	m.errs[path] = err
}

// Calls returns the recorded calls in order.
func (m *Mutator) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

func (m *Mutator) record(c Call) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, c)
	return m.errs[c.Path]
}

// ForceUnmount implements hostfs.Mutator: removes the topmost mount of path.
func (m *Mutator) ForceUnmount(_ context.Context, path string) error {
	if err := m.record(Call{Op: "ForceUnmount", Path: path}); err != nil {
		return err
	}
	m.mounts.Unmount(path)
	return nil
}

// Quarantine implements hostfs.Mutator: refuses mountpoints, moves the
// source Dir's aggregates into a new quarantine folder (collision suffix
// "-<n>") and leaves the source empty.
func (m *Mutator) Quarantine(_ context.Context, srcDir, destDir string) (hostfs.QuarantineMove, error) {
	if err := m.record(Call{Op: "Quarantine", Path: srcDir, Dest: destDir}); err != nil {
		return hostfs.QuarantineMove{}, err
	}
	if m.mounts.mountedAtOrBelow(srcDir) {
		return hostfs.QuarantineMove{}, fmt.Errorf("quarantine %s: %w", srcDir, hostfs.ErrIsMountpoint)
	}
	dir, ok := m.dirs.take(srcDir)
	if !ok {
		return hostfs.QuarantineMove{}, nil
	}
	dest := filepath.Clean(destDir)
	for n := 1; ; n++ {
		name := dest
		if n > 1 {
			name = dest + "-" + strconv.Itoa(n)
		}
		stats := hostfs.QuarantineStats{Entries: int64(dir.Entries), Files: dir.Files, Bytes: dir.Bytes,
			OldestModTime: dir.ModTime, NewestModTime: dir.ModTime}
		if m.quarantine.addIfAbsent(filepath.Base(name), stats) {
			return hostfs.QuarantineMove{Destination: filepath.Join(m.quarantine.Dir, filepath.Base(name)), Entries: dir.Entries}, nil
		}
	}
}
