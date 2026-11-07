package fake

import (
	"context"
	"slices"
	"sync"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// MountTable is a mutable in-memory mountinfo. Entries keep insertion order,
// so a later Mount on the same path is an overmount (last wins).
type MountTable struct {
	mu      sync.Mutex
	entries []hostfs.MountEntry
	nextID  int
	err     error
	reads   int
}

// Snapshot implements hostfs.MountTable.
func (t *MountTable) Snapshot(_ context.Context) (hostfs.MountSnapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reads++
	if t.err != nil {
		return hostfs.MountSnapshot{}, t.err
	}
	return hostfs.MountSnapshot{Entries: slices.Clone(t.entries)}, nil
}

// Mount appends an entry (an overmount if path is already mounted) and returns it.
func (t *MountTable) Mount(path, fstype, source, superOptions string) hostfs.MountEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	e := hostfs.MountEntry{ID: 1000 + t.nextID, ParentID: 1, MountPoint: path, Root: "/", FSType: fstype,
		Source: source, Options: "rw,relatime", SuperOptions: superOptions}
	t.entries = append(t.entries, e)
	return e
}

// Unmount removes the topmost (last) entry for path, like umount -l.
// It reports whether anything was mounted.
func (t *MountTable) Unmount(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.entries) - 1; i >= 0; i-- {
		if t.entries[i].MountPoint == path {
			t.entries = slices.Delete(t.entries, i, i+1)
			return true
		}
	}
	return false
}

// Set replaces all entries.
func (t *MountTable) Set(entries ...hostfs.MountEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries = slices.Clone(entries)
}

// SetError makes Snapshot fail with err (nil clears it).
func (t *MountTable) SetError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.err = err
}

// IsMountpoint reports whether path is currently mounted.
func (t *MountTable) IsMountpoint(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return hostfs.MountSnapshot{Entries: t.entries}.IsMountpoint(path)
}

// Reads returns how many snapshots were taken.
func (t *MountTable) Reads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reads
}

func (t *MountTable) mountedAtOrBelow(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.entries {
		if e.MountPoint == path || (len(e.MountPoint) > len(path) && e.MountPoint[:len(path)] == path && e.MountPoint[len(path)] == '/') {
			return true
		}
	}
	return false
}
