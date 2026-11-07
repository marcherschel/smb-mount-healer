package inventory

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Input is everything Build needs. All of it is gathered by the worker.
type Input struct {
	NodeName        string
	Layout          kubelet.Layout
	PVs             []corev1.PersistentVolume // all PVs; only those with Layout.DriverName are considered
	Pods            []corev1.Pod              // pods with spec.nodeName == NodeName
	Snapshot        hostfs.MountSnapshot
	PodVolumeDirs   []hostfs.PodVolumeDir
	GlobalmountDirs []hostfs.GlobalmountDir
	DirFacts        map[string]hostfs.DirFacts // keyed by path, for non-mountpoint dirs (see DirsToInspect)
	Now             time.Time
	PodStartupGrace time.Duration
}

// NodeView is the worker's model of all smb volumes on its node.
type NodeView struct {
	NodeName            string
	Volumes             []VolumeView            // sorted by PVName
	UnknownGlobalmounts []hostfs.GlobalmountDir // hash matches no smb PV (metric unknown_globalmounts; never touched)
}

// VolumeView is one smb PV on this node; it maps 1:1 to an SmbMount.
type VolumeView struct {
	PVName       string
	Share        string
	VolumeHandle string
	SmbMountName string
	Globalmount  GlobalmountView
	PodMounts    []PodMountView  // pods that reference the PV, sorted by UID
	OrphanDirs   []OrphanDirView // pod dirs whose UID has no pod, sorted by path
}

// GlobalmountView is the staging mount of a volume.
type GlobalmountView struct {
	Path         string
	Mounted      bool
	FSType       string
	SuperOptions string
	Dir          hostfs.DirFacts // only meaningful when !Mounted
}

// PodMountView is one pod's bind mount of the PV.
type PodMountView struct {
	Namespace   string
	Pod         string
	UID         types.UID
	Workload    string // display hint "Kind/name"
	Path        string
	Mounted     bool
	FSType      string
	Terminating bool
	// Skip is true when the pod is terminating or younger than PodStartupGrace; it is not probed.
	Skip       bool
	SkipReason string
}

// OrphanDirView is a pod volume dir with no live pod.
type OrphanDirView struct {
	UID     types.UID
	Path    string
	Mounted bool
	FSType  string
	Dir     hostfs.DirFacts // only meaningful when !Mounted
}

// ProbeTarget is one path the ProberRunnable should probe.
type ProbeTarget struct {
	SmbMountName string
	PVName       string
	Kind         v1alpha1.MountKind
	PodUID       types.UID // empty for globalmounts
	Path         string
}
