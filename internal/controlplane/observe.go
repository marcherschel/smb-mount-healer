package controlplane

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/remediation"
)

// podNodeNameField is the pod field selector the API server supports natively.
const podNodeNameField = "spec.nodeName"

// ownerMemoTTL is how long a resolved pod owner is kept after the pod was last seen.
const ownerMemoTTL = time.Hour

// observe builds the Observation for one Step. Terminal remediations only
// need the SmbMounts (Step mirrors) and their own SmbHostTasks (Step copies
// task phases into status.hostTasks), so nothing else is read for them.
func (e *Engine) observe(ctx context.Context, r *v1alpha1.SmbMountRemediation) (remediation.Observation, error) {
	node := r.Spec.NodeName
	obs := remediation.Observation{
		Now:      e.clock.Now(),
		Aborted:  r.Annotations[v1alpha1.AnnotationAbort] == "true",
		Deleting: r.DeletionTimestamp != nil,
	}
	mounts, err := e.nodeSmbMounts(ctx, node)
	if err != nil {
		return obs, err
	}
	obs.SmbMounts = mounts

	// Also for terminal remediations: a task can complete after the
	// remediation finished (the worker publishes the probe round that
	// reflects a ProbeNow before it marks the task Succeeded, and that round
	// can be what lets verifying finish), and its phase must still reach
	// status.hostTasks.
	var tasks v1alpha1.SmbHostTaskList
	if err := e.c.List(ctx, &tasks, client.MatchingLabels{v1alpha1.LabelRemediation: r.Name}); err != nil {
		return obs, fmt.Errorf("list host tasks: %w", err)
	}
	for _, t := range tasks.Items {
		if owned(&t, r) {
			obs.HostTasks = append(obs.HostTasks, t)
		}
	}
	if r.Status.Stage.Terminal() {
		return obs, nil
	}

	var rems v1alpha1.SmbMountRemediationList
	if err := e.c.List(ctx, &rems); err != nil {
		return obs, fmt.Errorf("list remediations: %w", err)
	}
	for _, o := range rems.Items {
		if o.Name != r.Name && !o.Status.Stage.Terminal() {
			obs.OtherActive = append(obs.OtherActive, o)
		}
	}

	// The scoped PVs count as smb PVs even when the node has no SmbMount for
	// them any more: after the episode unstaged a volume and the worker
	// deleted its SmbMount, a replacement pod that cannot mount it yet
	// (ContainerCreating during an outage) must still be seen, or verifying
	// would take its workload for "elsewhere".
	var scopePVs []string
	if r.Status.Scope != nil {
		scopePVs = r.Status.Scope.PVs
	}
	if obs.Pods, err = e.nodePods(ctx, node, mounts, scopePVs...); err != nil {
		return obs, err
	}

	drivers, err := e.kc.DriverPods(ctx, node)
	if err != nil {
		return obs, err
	}
	for i := range drivers {
		p := &drivers[i]
		// A terminating driver pod is already on its way out: it is neither
		// deleted again nor a candidate for the restart.
		if p.DeletionTimestamp != nil {
			continue
		}
		obs.DriverPods = append(obs.DriverPods, remediation.DriverPodObservation{
			Namespace: p.Namespace, Name: p.Name, UID: p.UID,
			Running: p.Status.Phase == corev1.PodRunning, Ready: podReady(p),
		})
	}

	refs := map[string]v1alpha1.WorkloadRef{}
	if r.Status.Scope != nil {
		for _, w := range r.Status.Scope.Workloads {
			refs[w.String()] = w.WorkloadRef
		}
	}
	for _, p := range obs.Pods {
		if p.Owner != nil {
			refs[p.Owner.String()] = *p.Owner
		}
	}
	if obs.Workloads, err = e.workloads(ctx, refs); err != nil {
		return obs, err
	}
	return obs, nil
}

func owned(t *v1alpha1.SmbHostTask, r *v1alpha1.SmbMountRemediation) bool {
	for _, o := range t.OwnerReferences {
		if o.UID == r.UID {
			return true
		}
	}
	return len(t.OwnerReferences) == 0
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeSmbMounts returns the SmbMounts of node (from the cache), sorted by name.
func (e *Engine) nodeSmbMounts(ctx context.Context, node string) ([]v1alpha1.SmbMount, error) {
	var list v1alpha1.SmbMountList
	if err := e.c.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("list SmbMounts: %w", err)
	}
	out := make([]v1alpha1.SmbMount, 0, len(list.Items))
	for _, sm := range list.Items {
		if sm.Spec.NodeName == node {
			out = append(out, sm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// workloads reads every referenced workload live.
func (e *Engine) workloads(ctx context.Context, refs map[string]v1alpha1.WorkloadRef) ([]remediation.WorkloadObservation, error) {
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]remediation.WorkloadObservation, 0, len(keys))
	for _, k := range keys {
		ref := refs[k]
		st, err := e.kc.Workload(ctx, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, remediation.WorkloadObservation{Ref: ref, Exists: st.Exists, Replicas: st.Replicas, ReadyReplicas: st.ReadyReplicas})
	}
	return out, nil
}

// nodePods lists the pods on node live (field selector spec.nodeName; the
// control plane has no cluster-wide pod informer) and keeps those that use
// one of the node's smb PVs. A pod uses a PV if one of its
// persistentVolumeClaim (or generic ephemeral) volumes is bound to it, or
// if the worker reports a pod mount of that PV for the pod's UID. Succeeded
// and Failed pods hold no mounts and are left out. extraPVs are smb PVs
// known from elsewhere (a remediation's scope) that may have no SmbMount on
// the node at the moment.
func (e *Engine) nodePods(ctx context.Context, node string, mounts []v1alpha1.SmbMount, extraPVs ...string) ([]remediation.PodObservation, error) {
	smbPVs := map[string]bool{}
	for _, pv := range extraPVs {
		smbPVs[pv] = true
	}
	reported := map[types.UID][]string{}
	for _, sm := range mounts {
		smbPVs[sm.Spec.PVName] = true
		for _, pm := range sm.Status.PodMounts {
			reported[pm.UID] = append(reported[pm.UID], sm.Spec.PVName)
		}
	}
	var list corev1.PodList
	if err := e.live.List(ctx, &list, client.MatchingFields{podNodeNameField: node}); err != nil {
		return nil, fmt.Errorf("list pods on node %s: %w", node, err)
	}
	now := e.clock.Now()
	var out []remediation.PodObservation
	for i := range list.Items {
		p := &list.Items[i]
		if p.Spec.NodeName != node || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		pvs := append([]string(nil), reported[p.UID]...)
		for _, v := range p.Spec.Volumes {
			claim := ""
			switch {
			case v.PersistentVolumeClaim != nil:
				claim = v.PersistentVolumeClaim.ClaimName
			case v.Ephemeral != nil:
				claim = p.Name + "-" + v.Name
			}
			if claim == "" {
				continue
			}
			var pvc corev1.PersistentVolumeClaim
			if err := e.c.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: claim}, &pvc); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("get PVC %s/%s: %w", p.Namespace, claim, err)
			}
			if smbPVs[pvc.Spec.VolumeName] {
				pvs = append(pvs, pvc.Spec.VolumeName)
			}
		}
		if len(pvs) == 0 {
			continue
		}
		owner, err := e.owners.resolve(ctx, e.kc, p, now)
		if err != nil {
			return nil, err
		}
		out = append(out, remediation.PodObservation{
			Namespace: p.Namespace, Name: p.Name, UID: p.UID, NodeName: p.Spec.NodeName, PVs: pvs,
			Owner: owner, Terminating: p.DeletionTimestamp != nil, Phase: p.Status.Phase,
		})
	}
	e.owners.prune(now)
	return out, nil
}

// ownerMemo caches resolved pod owners by pod UID: a pod's controller and
// its ReplicaSet's Deployment never change, so the live ReplicaSet read of
// kube.Cluster.ResolveOwner happens once per pod instead of once per poll.
type ownerMemo struct {
	mu sync.Mutex
	m  map[types.UID]ownerEntry
}

type ownerEntry struct {
	ref  v1alpha1.WorkloadRef
	seen time.Time
}

func newOwnerMemo() *ownerMemo { return &ownerMemo{m: map[types.UID]ownerEntry{}} }

func (o *ownerMemo) resolve(ctx context.Context, kc kube.Cluster, p *corev1.Pod, now time.Time) (*v1alpha1.WorkloadRef, error) {
	o.mu.Lock()
	if e, ok := o.m[p.UID]; ok {
		e.seen = now
		o.m[p.UID] = e
		o.mu.Unlock()
		ref := e.ref
		return &ref, nil
	}
	o.mu.Unlock()
	ref, err := kc.ResolveOwner(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("resolve owner of pod %s/%s: %w", p.Namespace, p.Name, err)
	}
	if ref != nil && p.UID != "" {
		o.mu.Lock()
		o.m[p.UID] = ownerEntry{ref: *ref, seen: now}
		o.mu.Unlock()
	}
	return ref, nil
}

func (o *ownerMemo) prune(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for uid, e := range o.m {
		if now.Sub(e.seen) > ownerMemoTTL {
			delete(o.m, uid)
		}
	}
}
