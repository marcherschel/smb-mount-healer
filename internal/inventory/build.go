package inventory

import (
	"cmp"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// Skip reasons for PodMountView.SkipReason.
const (
	// SkipTerminating: the pod has a deletionTimestamp.
	SkipTerminating = "terminating"
	// SkipStartupGrace: the pod is younger than PodStartupGrace (age is
	// measured from status.startTime, else metadata.creationTimestamp).
	SkipStartupGrace = "startup_grace"
	// SkipPodFinished: the pod is Succeeded/Failed; kubelet tears its
	// volumes down, so a missing mount is expected, not a fault.
	SkipPodFinished = "pod_finished"
)

// podTemplateHashLabel is set by the Deployment controller on its pods and
// is the suffix of the owning ReplicaSet's name.
const podTemplateHashLabel = "pod-template-hash"

// pvInfo is one smb PV.
type pvInfo struct {
	name, share, handle, hash, gmPath string
}

// podRef is one pod referencing an smb PV.
type podRef struct {
	pod *corev1.Pod
	pv  string
}

// DirsToInspect returns, sorted, the listed pod volume dirs and globalmount
// dirs that are NOT mountpoints in snap: the only dirs the worker may read.
func DirsToInspect(snap hostfs.MountSnapshot, pods []hostfs.PodVolumeDir, gms []hostfs.GlobalmountDir) []string {
	mounted := mountpointSet(snap)
	out := make([]string, 0, len(pods)+len(gms))
	for _, d := range pods {
		if d.Path != "" && !mounted[d.Path] {
			out = append(out, d.Path)
		}
	}
	for _, d := range gms {
		if d.Path != "" && !mounted[d.Path] {
			out = append(out, d.Path)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Build assembles the NodeView. Deterministic: same input, same output,
// independent of the order of PVs, Pods, PodVolumeDirs and GlobalmountDirs
// (Snapshot order is meaningful: the last entry for a mountpoint wins).
//
// A smb PV (spec.csi.driver == Layout.DriverName, non-empty volumeHandle)
// gets a VolumeView when a pod on the node references it (via the PV's
// claimRef), its globalmount is mounted or its directory exists (listed in
// GlobalmountDirs or DirFacts.Exists), or orphan pod dirs of it exist.
// Pod volume dirs of PVs that are not smb PVs (other drivers, deleted PVs)
// are ignored.
func Build(in Input) NodeView {
	pvs := smbPVs(in)
	byName := make(map[string]*pvInfo, len(pvs))
	byHash := make(map[string]bool, len(pvs))
	for i := range pvs {
		byName[pvs[i].name] = &pvs[i]
		byHash[pvs[i].hash] = true
	}
	refs := podRefs(in, pvs)

	referenced := make(map[string]map[types.UID]bool) // pv -> pod UIDs
	podMounts := make(map[string][]PodMountView)
	for _, r := range refs {
		if referenced[r.pv] == nil {
			referenced[r.pv] = make(map[types.UID]bool)
		}
		referenced[r.pv][r.pod.UID] = true
		podMounts[r.pv] = append(podMounts[r.pv], podMountView(in, r))
	}

	orphans := make(map[string][]OrphanDirView)
	seenOrphan := make(map[string]bool)
	for _, d := range in.PodVolumeDirs {
		if byName[d.PVName] == nil || referenced[d.PVName][d.PodUID] || seenOrphan[d.Path] {
			continue
		}
		seenOrphan[d.Path] = true
		o := OrphanDirView{UID: d.PodUID, Path: d.Path}
		if e, ok := in.Snapshot.Lookup(d.Path); ok {
			o.Mounted, o.FSType = true, e.FSType
		} else {
			o.Dir = in.DirFacts[d.Path]
		}
		orphans[d.PVName] = append(orphans[d.PVName], o)
	}

	listedGM := make(map[string]bool, len(in.GlobalmountDirs))
	for _, d := range in.GlobalmountDirs {
		listedGM[d.Hash] = true
	}

	view := NodeView{NodeName: in.NodeName}
	for i := range pvs {
		pv := &pvs[i]
		gm := globalmountView(in, pv, listedGM[pv.hash])
		pm, od := podMounts[pv.name], orphans[pv.name]
		if len(pm) == 0 && len(od) == 0 && !gm.Mounted && !gm.Dir.Exists {
			continue
		}
		slices.SortFunc(pm, func(a, b PodMountView) int {
			return cmp.Or(cmp.Compare(a.UID, b.UID), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Pod, b.Pod))
		})
		slices.SortFunc(od, func(a, b OrphanDirView) int { return cmp.Compare(a.Path, b.Path) })
		view.Volumes = append(view.Volumes, VolumeView{
			PVName:       pv.name,
			Share:        pv.share,
			VolumeHandle: pv.handle,
			SmbMountName: v1alpha1.SmbMountName(in.NodeName, pv.name),
			Globalmount:  gm,
			PodMounts:    pm,
			OrphanDirs:   od,
		})
	}
	view.UnknownGlobalmounts = unknownGlobalmounts(in, byHash)
	return view
}

// smbPVs returns the smb PVs sorted by name (first wins on duplicate names).
func smbPVs(in Input) []pvInfo {
	out := make([]pvInfo, 0, len(in.PVs))
	for i := range in.PVs {
		pv := &in.PVs[i]
		csi := pv.Spec.CSI
		if csi == nil || csi.Driver != in.Layout.DriverName || csi.VolumeHandle == "" || pv.Name == "" {
			continue
		}
		out = append(out, pvInfo{
			name:   pv.Name,
			share:  csi.VolumeAttributes["source"],
			handle: csi.VolumeHandle,
			hash:   kubelet.GlobalmountHash(csi.VolumeHandle),
			gmPath: in.Layout.GlobalmountPath(csi.VolumeHandle),
		})
	}
	slices.SortFunc(out, func(a, b pvInfo) int {
		return cmp.Or(cmp.Compare(a.name, b.name), cmp.Compare(a.handle, b.handle), cmp.Compare(a.share, b.share))
	})
	return slices.CompactFunc(out, func(a, b pvInfo) bool { return a.name == b.name })
}

// podRefs returns one entry per (pod on this node, smb PV it references via
// a PVC), sorted by PV, pod UID. PVs are matched through spec.claimRef; when
// several PVs claim the same PVC, the lowest PV name wins.
func podRefs(in Input, pvs []pvInfo) []podRef {
	pvByClaim := make(map[string]string)
	smb := make(map[string]bool, len(pvs))
	for _, p := range pvs {
		smb[p.name] = true
	}
	for i := range in.PVs {
		pv := &in.PVs[i]
		cr := pv.Spec.ClaimRef
		if cr == nil || cr.Name == "" || !smb[pv.Name] {
			continue
		}
		key := cr.Namespace + "/" + cr.Name
		if cur, ok := pvByClaim[key]; !ok || pv.Name < cur {
			pvByClaim[key] = pv.Name
		}
	}

	var out []podRef
	seen := make(map[string]bool)
	for i := range in.Pods {
		pod := &in.Pods[i]
		if in.NodeName != "" && pod.Spec.NodeName != in.NodeName {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			var claim string
			switch {
			case v.PersistentVolumeClaim != nil:
				claim = v.PersistentVolumeClaim.ClaimName
			case v.Ephemeral != nil:
				claim = pod.Name + "-" + v.Name
			default:
				continue
			}
			pv, ok := pvByClaim[pod.Namespace+"/"+claim]
			key := pv + "\x00" + string(pod.UID)
			if !ok || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, podRef{pod: pod, pv: pv})
		}
	}
	return out
}

func podMountView(in Input, r podRef) PodMountView {
	pod := r.pod
	p := in.Layout.PodVolumeMountPath(pod.UID, r.pv)
	v := PodMountView{
		Namespace:   pod.Namespace,
		Pod:         pod.Name,
		UID:         pod.UID,
		Workload:    workloadOf(pod),
		Path:        p,
		Terminating: pod.DeletionTimestamp != nil,
	}
	if e, ok := in.Snapshot.Lookup(p); ok {
		v.Mounted, v.FSType = true, e.FSType
	}
	start := pod.CreationTimestamp.Time
	if pod.Status.StartTime != nil {
		start = pod.Status.StartTime.Time
	}
	switch {
	case v.Terminating:
		v.SkipReason = SkipTerminating
	case pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed:
		v.SkipReason = SkipPodFinished
	case in.Now.Sub(start) < in.PodStartupGrace:
		v.SkipReason = SkipStartupGrace
	}
	v.Skip = v.SkipReason != ""
	return v
}

// workloadOf derives the display hint "Kind/name" from the pod's controller
// ownerReference (else its first ownerReference). A ReplicaSet owner whose
// name is "<deployment>-<pod-template-hash>" (hash from the pod's label) is
// reported as "Deployment/<deployment>"; other ReplicaSets stay
// "ReplicaSet/<name>". Pods without owners yield "". This is a hint only;
// the control plane resolves owners authoritatively.
func workloadOf(pod *corev1.Pod) string {
	refs := pod.OwnerReferences
	if len(refs) == 0 {
		return ""
	}
	ref := refs[0]
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			ref = r
			break
		}
	}
	if ref.Kind == "ReplicaSet" {
		if h := pod.Labels[podTemplateHashLabel]; h != "" {
			if dep, ok := strings.CutSuffix(ref.Name, "-"+h); ok && dep != "" {
				return "Deployment/" + dep
			}
		}
	}
	return ref.Kind + "/" + ref.Name
}

// globalmountView builds the staging-mount view of pv. When the dir is
// listed but has no DirFacts (inspection failed or raced), it is reported
// as existing and empty: Build never claims shadow data it has not seen.
func globalmountView(in Input, pv *pvInfo, listed bool) GlobalmountView {
	gm := GlobalmountView{Path: pv.gmPath}
	if e, ok := in.Snapshot.Lookup(pv.gmPath); ok {
		gm.Mounted, gm.FSType, gm.SuperOptions = true, e.FSType, e.SuperOptions
		return gm
	}
	if f, ok := in.DirFacts[pv.gmPath]; ok {
		gm.Dir = f
	} else if listed {
		gm.Dir = hostfs.DirFacts{Exists: true, IsDir: true, Empty: true}
	}
	return gm
}

// unknownGlobalmounts returns listed or mounted globalmount dirs of the
// driver whose hash matches no smb PV, sorted by hash then path.
func unknownGlobalmounts(in Input, known map[string]bool) []hostfs.GlobalmountDir {
	var out []hostfs.GlobalmountDir
	for _, d := range in.GlobalmountDirs {
		if !known[d.Hash] {
			out = append(out, d)
		}
	}
	prefix := in.Layout.StagingRoot() + "/"
	for _, e := range in.Snapshot.Entries {
		rest, ok := strings.CutPrefix(e.MountPoint, prefix)
		if !ok {
			continue
		}
		hash, tail, ok := strings.Cut(rest, "/")
		if !ok || tail != "globalmount" || hash == "" || known[hash] {
			continue
		}
		out = append(out, hostfs.GlobalmountDir{Hash: hash, Path: path.Clean(e.MountPoint)})
	}
	slices.SortFunc(out, func(a, b hostfs.GlobalmountDir) int {
		return cmp.Or(cmp.Compare(a.Hash, b.Hash), cmp.Compare(a.Path, b.Path))
	})
	return slices.Compact(out)
}

func mountpointSet(snap hostfs.MountSnapshot) map[string]bool {
	m := make(map[string]bool, len(snap.Entries))
	for _, e := range snap.Entries {
		m[e.MountPoint] = true
	}
	return m
}

// ProbeTargets lists the mounts to probe, sorted by path (then PV, kind,
// pod UID): every mounted globalmount (Kind globalmount), every non-skipped
// pod mount (Kind pod; probed even when not mounted, which is how
// not_mountpoint is detected) and every mounted orphan pod dir (Kind pod,
// PodUID = the orphan dir's UID).
func (v NodeView) ProbeTargets() []ProbeTarget {
	var out []ProbeTarget
	for _, vol := range v.Volumes {
		base := ProbeTarget{SmbMountName: vol.SmbMountName, PVName: vol.PVName}
		if vol.Globalmount.Mounted {
			t := base
			t.Kind, t.Path = v1alpha1.MountKindGlobalmount, vol.Globalmount.Path
			out = append(out, t)
		}
		for _, pm := range vol.PodMounts {
			if pm.Skip {
				continue
			}
			t := base
			t.Kind, t.PodUID, t.Path = v1alpha1.MountKindPod, pm.UID, pm.Path
			out = append(out, t)
		}
		for _, od := range vol.OrphanDirs {
			if !od.Mounted {
				continue
			}
			t := base
			t.Kind, t.PodUID, t.Path = v1alpha1.MountKindPod, od.UID, od.Path
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b ProbeTarget) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.PVName, b.PVName),
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.PodUID, b.PodUID))
	})
	return out
}
