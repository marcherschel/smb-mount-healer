package remediation

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

// index is a sorted, de-duplicated view of an Observation, so every output
// is independent of the order of the observation's slices.
type index struct {
	mountByName map[string]*v1alpha1.SmbMount
	mountByPV   map[string]*v1alpha1.SmbMount
	mountNames  []string         // sorted
	pods        []PodObservation // sorted by namespace, name, uid
	podByUID    map[types.UID]*PodObservation
	workloads   map[string]WorkloadObservation
	driverPods  []DriverPodObservation // sorted by namespace, name, uid
	hostTasks   map[string]*v1alpha1.SmbHostTask
	others      []v1alpha1.SmbMountRemediation // other non-terminal remediations, sorted by name
}

func buildIndex(obj *v1alpha1.SmbMountRemediation, obs Observation) index {
	idx := index{
		mountByName: map[string]*v1alpha1.SmbMount{},
		mountByPV:   map[string]*v1alpha1.SmbMount{},
		podByUID:    map[types.UID]*PodObservation{},
		workloads:   map[string]WorkloadObservation{},
		hostTasks:   map[string]*v1alpha1.SmbHostTask{},
	}
	node := obj.Spec.NodeName
	for i := range obs.SmbMounts {
		sm := &obs.SmbMounts[i]
		if sm.Spec.NodeName != "" && sm.Spec.NodeName != node {
			continue
		}
		if prev, ok := idx.mountByName[sm.Name]; ok && prev.ResourceVersion >= sm.ResourceVersion {
			continue
		}
		idx.mountByName[sm.Name] = sm
	}
	for name := range idx.mountByName {
		idx.mountNames = append(idx.mountNames, name)
	}
	sort.Strings(idx.mountNames)
	for _, name := range idx.mountNames {
		sm := idx.mountByName[name]
		if _, ok := idx.mountByPV[sm.Spec.PVName]; !ok {
			idx.mountByPV[sm.Spec.PVName] = sm
		}
	}

	idx.pods = sortedPods(obs.Pods, node)
	for i := range idx.pods {
		idx.podByUID[idx.pods[i].UID] = &idx.pods[i]
	}

	for _, w := range obs.Workloads {
		k := w.Ref.String()
		if prev, ok := idx.workloads[k]; ok && lessWorkloadObs(w, prev) {
			continue
		}
		idx.workloads[k] = w
	}

	idx.driverPods = append([]DriverPodObservation(nil), obs.DriverPods...)
	sort.Slice(idx.driverPods, func(i, j int) bool {
		a, b := idx.driverPods[i], idx.driverPods[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.UID < b.UID
	})

	for i := range obs.HostTasks {
		t := &obs.HostTasks[i]
		idx.hostTasks[t.Name] = t
	}

	for _, r := range obs.OtherActive {
		if r.Name == obj.Name || r.Status.Stage.Terminal() {
			continue
		}
		idx.others = append(idx.others, r)
	}
	sort.Slice(idx.others, func(i, j int) bool { return idx.others[i].Name < idx.others[j].Name })
	return idx
}

// lessWorkloadObs orders duplicate workload observations deterministically
// (the "larger" one wins).
func lessWorkloadObs(a, b WorkloadObservation) bool {
	if a.Exists != b.Exists {
		return !a.Exists
	}
	if a.Replicas != b.Replicas {
		return a.Replicas < b.Replicas
	}
	return a.ReadyReplicas < b.ReadyReplicas
}

// sortedPods returns the pods on node (or with no node set), with sorted
// PVs, ordered by namespace, name and UID, deduplicated by UID.
func sortedPods(in []PodObservation, node string) []PodObservation {
	out := make([]PodObservation, 0, len(in))
	seen := map[types.UID]bool{}
	for _, p := range in {
		if p.NodeName != "" && node != "" && p.NodeName != node {
			continue
		}
		p.PVs = sortedUnique(p.PVs)
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.UID != b.UID {
			return a.UID < b.UID
		}
		return podRank(a) < podRank(b)
	})
	dedup := out[:0]
	for _, p := range out {
		if seen[p.UID] {
			continue
		}
		seen[p.UID] = true
		dedup = append(dedup, p)
	}
	return dedup
}

// podRank orders duplicate pod observations (same UID) deterministically.
func podRank(p PodObservation) string {
	owner := ""
	if p.Owner != nil {
		owner = p.Owner.String()
	}
	t := "0"
	if p.Terminating {
		t = "1"
	}
	return t + "/" + string(p.Phase) + "/" + owner + "/" + strings.Join(p.PVs, ",")
}

func (x index) workload(ref v1alpha1.WorkloadRef) (WorkloadObservation, bool) {
	o, ok := x.workloads[ref.String()]
	return o, ok
}

// podsUsingPV returns the pods on the node that mount pv.
func (x index) podsUsingPV(pv string) []PodObservation {
	var out []PodObservation
	for _, p := range x.pods {
		if containsString(p.PVs, pv) {
			out = append(out, p)
		}
	}
	return out
}

// pvReferenced reports whether any pod on the node mounts pv.
func (x index) pvReferenced(pv string) bool { return len(x.podsUsingPV(pv)) > 0 }

// globalmountHealthy reports whether pv's staging mount is a mounted,
// Healthy mount. A missing SmbMount or globalmount status counts as not healthy.
func (x index) globalmountHealthy(pv string) bool {
	sm := x.mountByPV[pv]
	if sm == nil || sm.Status.Globalmount == nil {
		return false
	}
	return sm.Status.Globalmount.Mounted && sm.Status.Health == v1alpha1.HealthHealthy
}

// podVolumePath derives <kubelet>/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount
// from the SmbMount's globalmount path
// (<kubelet>/plugins/kubernetes.io/csi/<driver>/<hash>/globalmount).
func podVolumePath(sm *v1alpha1.SmbMount, uid types.UID) string {
	gm := sm.Spec.GlobalmountPath
	i := strings.Index(gm, "/plugins/kubernetes.io/csi/")
	if i < 0 {
		return ""
	}
	root := gm[:i]
	if root == "" {
		root = "/"
	}
	return kubelet.Layout{Root: root}.PodVolumeMountPath(uid, sm.Spec.PVName)
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 0
	for i, s := range out {
		if i > 0 && s == out[j-1] {
			continue
		}
		out[j] = s
		j++
	}
	return out[:j]
}
