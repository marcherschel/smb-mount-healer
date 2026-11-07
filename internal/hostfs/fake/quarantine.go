package fake

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"

	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
)

// QuarantineInspector implements hostfs.QuarantineInspector over in-memory
// folders under Dir. Tests and the simulator change the "disk" with
// SetFolder/RemoveFolder (e.g. a human migrating data away).
type QuarantineInspector struct {
	Dir string

	mu      sync.Mutex
	folders map[string]hostfs.QuarantineStats
	scanErr error
}

// SetFolder creates or replaces folder name with stats.
func (q *QuarantineInspector) SetFolder(name string, stats hostfs.QuarantineStats) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.folders == nil {
		q.folders = map[string]hostfs.QuarantineStats{}
	}
	q.folders[name] = stats
}

// RemoveFolder removes folder name (data moved away by a human).
func (q *QuarantineInspector) RemoveFolder(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.folders, name)
}

// SetScanError makes every Scan fail with err (nil clears it).
func (q *QuarantineInspector) SetScanError(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.scanErr = err
}

func (q *QuarantineInspector) addIfAbsent(name string, stats hostfs.QuarantineStats) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.folders[name]; ok {
		return false
	}
	if q.folders == nil {
		q.folders = map[string]hostfs.QuarantineStats{}
	}
	q.folders[name] = stats
	return true
}

// ListFolders implements hostfs.QuarantineInspector (sorted by name; nil if none).
func (q *QuarantineInspector) ListFolders(_ context.Context) ([]hostfs.QuarantineFolder, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []hostfs.QuarantineFolder
	for name := range q.folders {
		out = append(out, hostfs.QuarantineFolder{Name: name, Path: filepath.Join(q.Dir, name)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Scan implements hostfs.QuarantineInspector. A missing folder wraps fs.ErrNotExist.
func (q *QuarantineInspector) Scan(_ context.Context, folderPath string) (hostfs.QuarantineStats, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.scanErr != nil {
		return hostfs.QuarantineStats{}, q.scanErr
	}
	st, ok := q.folders[filepath.Base(folderPath)]
	if !ok || filepath.Dir(filepath.Clean(folderPath)) != filepath.Clean(q.Dir) {
		return hostfs.QuarantineStats{}, fmt.Errorf("scan %s: %w", folderPath, fs.ErrNotExist)
	}
	return st, nil
}
