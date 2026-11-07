package hostfs

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// Sentinel errors.
var (
	// ErrIsMountpoint is returned when an operation that must only touch
	// local directories finds the path is (or became) a mountpoint.
	ErrIsMountpoint = errors.New("hostfs: path is a mountpoint")
	// ErrHung is returned when a previous child for the same path is still stuck.
	ErrHung = errors.New("hostfs: previous operation on path is hung")
)

// MountEntry is one line of /proc/self/mountinfo.
type MountEntry struct {
	ID           int
	ParentID     int
	MountPoint   string
	Root         string
	FSType       string
	Source       string
	Options      string // per-mount options
	SuperOptions string // per-superblock options (e.g. contains "soft" for cifs)
}

// MountSnapshot is one consistent read of mountinfo, in file order.
type MountSnapshot struct {
	Entries []MountEntry
}

// MountTable reads mountinfo.
type MountTable interface {
	Snapshot(ctx context.Context) (MountSnapshot, error)
}

// DirFacts describes a directory that is NOT a mountpoint.
type DirFacts struct {
	Exists bool
	IsDir  bool
	Empty  bool
}

// PodVolumeDir is <kubelet>/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount.
type PodVolumeDir struct {
	PodUID types.UID
	PVName string
	Path   string
}

// GlobalmountDir is <kubelet>/plugins/kubernetes.io/csi/<driver>/<hash>/globalmount.
type GlobalmountDir struct {
	Hash string
	Path string
}

// DirInspector reads the kubelet directory tree on local disk. It never
// descends into a path that is a mountpoint.
type DirInspector interface {
	// Inspect reports facts about a non-mountpoint directory. It returns
	// ErrIsMountpoint if a fresh mountinfo read shows path is a mountpoint.
	Inspect(ctx context.Context, path string) (DirFacts, error)
	// ListPodVolumeDirs lists all CSI pod volume mount dirs on the node.
	ListPodVolumeDirs(ctx context.Context) ([]PodVolumeDir, error)
	// ListGlobalmountDirs lists all globalmount dirs of the configured CSI driver.
	ListGlobalmountDirs(ctx context.Context) ([]GlobalmountDir, error)
}

// ProbeClass classifies one probe. Failure values equal v1alpha1.FailureReason strings.
type ProbeClass string

const (
	ProbeOK            ProbeClass = "ok"
	ProbeNotMountpoint ProbeClass = "not_mountpoint"
	ProbeWrongFSType   ProbeClass = "wrong_fstype"
	ProbeStatError     ProbeClass = "stat_error"
	ProbeStatTimeout   ProbeClass = "stat_timeout"
	ProbeHung          ProbeClass = "probe_hung"
	ProbeError         ProbeClass = "probe_error"
)

// ProbeResult is the outcome of probing one mount path.
type ProbeResult struct {
	Path   string
	Class  ProbeClass
	FSType string
	// Detail is a short diagnostic (errno text, exit status). Never a directory listing.
	Detail   string
	Duration time.Duration
}

// Prober checks one mount: mountinfo presence and fstype from snap, then
// stat/statfs in a child process bounded by the configured timeout.
type Prober interface {
	// Probe never blocks beyond its timeout. If a previous child for path is
	// still stuck it returns ProbeHung without spawning a new one.
	Probe(ctx context.Context, path string, snap MountSnapshot) ProbeResult
	// Hung returns the number of currently hung children (metric probes_hung).
	Hung() int
}

// QuarantineMove is the result of a Quarantine call.
type QuarantineMove struct {
	// Destination is the created quarantine folder (healer-owned path).
	Destination string
	// Entries is the number of top-level entries moved.
	Entries int
}

// Mutator performs the only host mutations the healer does.
type Mutator interface {
	// ForceUnmount runs umount2(MNT_FORCE|MNT_DETACH) in a child with a timeout.
	ForceUnmount(ctx context.Context, path string) error
	// Quarantine moves every top-level entry of srcDir into destDir (created).
	// It refuses with ErrIsMountpoint if srcDir is a mountpoint (fresh check)
	// and never deletes anything. An empty srcDir moves nothing and creates nothing.
	Quarantine(ctx context.Context, srcDir, destDir string) (QuarantineMove, error)
}

// QuarantineFolder is one healer-created folder directly under QUARANTINE_DIR.
// Name is the folder's basename ("<pv>-<UTC>"), never a name from user data.
type QuarantineFolder struct {
	Name string
	Path string
}

// QuarantineStats are aggregates of one quarantine folder. By design there
// is no field that can carry an entry name.
type QuarantineStats struct {
	Entries       int64 // top-level entries
	Files         int64 // regular files seen by the walk
	Bytes         int64 // total size of regular files seen
	SizeTruncated bool  // walk budget (entries or time) exhausted
	OldestModTime time.Time
	NewestModTime time.Time
}

// QuarantineInspector lists and measures quarantine folders. Walks are
// bounded (max entries, time budget) and never follow symlinks.
type QuarantineInspector interface {
	// ListFolders returns the folders under QUARANTINE_DIR. A missing root is nil, nil.
	ListFolders(ctx context.Context) ([]QuarantineFolder, error)
	// Scan walks one folder and returns aggregates only.
	Scan(ctx context.Context, folderPath string) (QuarantineStats, error)
}
