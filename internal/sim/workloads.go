package sim

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// Workload is a simulated Deployment, StatefulSet or bare Pod with the
// replica controller emulated: pods are created (new UIDs) for missing
// replicas and deleted for surplus ones, and are placed on the node of
// Nodes with the fewest pods of the workload.
type Workload struct {
	w        *World
	Kind     v1alpha1.WorkloadKind // "" = bare pod
	Name     string
	PVCs     []string
	Nodes    []string
	Baseline int32 // the replicas a human wants; invariant target

	uid    string
	rsName string
	rsUID  string
	podSeq int
}

const podTemplateHash = "5d8f7c9b6"

// AddDeployment adds a Deployment fixture.
func (w *World) AddDeployment(name string, replicas int32, nodes []string, pvcs ...string) *Workload {
	return w.addWorkload(v1alpha1.WorkloadKindDeployment, name, replicas, nodes, pvcs)
}

// AddStatefulSet adds a StatefulSet fixture.
func (w *World) AddStatefulSet(name string, replicas int32, nodes []string, pvcs ...string) *Workload {
	return w.addWorkload(v1alpha1.WorkloadKindStatefulSet, name, replicas, nodes, pvcs)
}

// AddBarePod adds an unowned pod fixture.
func (w *World) AddBarePod(name, node string, pvcs ...string) *Workload {
	return w.addWorkload("", name, 1, []string{node}, pvcs)
}

func (w *World) addWorkload(kind v1alpha1.WorkloadKind, name string, replicas int32, nodes, pvcs []string) *Workload {
	if len(nodes) == 0 {
		nodes = []string{w.Nodes[0].Name}
	}
	wl := &Workload{w: w, Kind: kind, Name: name, PVCs: pvcs, Nodes: nodes, Baseline: replicas}
	w.workloads = append(w.workloads, wl)
	return wl
}

// Workload returns a workload fixture by name.
func (w *World) Workload(name string) *Workload {
	for _, wl := range w.workloads {
		if wl.Name == name {
			return wl
		}
	}
	return nil
}

func (wl *Workload) ref() string {
	if wl.Kind == "" {
		return "Pod/" + AppNS + "/" + wl.Name
	}
	return string(wl.Kind) + "/" + AppNS + "/" + wl.Name
}

func (wl *Workload) labels() map[string]string { return map[string]string{"app": wl.Name} }

func (wl *Workload) template() corev1.PodTemplateSpec {
	var vols []corev1.Volume
	for _, c := range wl.PVCs {
		vols = append(vols, corev1.Volume{Name: c, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c},
		}})
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: wl.labels()},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "busybox",
				Command: []string{"sh", "-c", "while true; do date > /data/hb-$POD_NAME; sleep 2; done"}}},
			Volumes:                       vols,
			TerminationGracePeriodSeconds: ptr.To(int64(podTerminationGrace / time.Second)),
		},
	}
}

func (wl *Workload) create(ctx context.Context) error {
	w := wl.w
	sel := &metav1.LabelSelector{MatchLabels: wl.labels()}
	switch wl.Kind {
	case v1alpha1.WorkloadKindDeployment:
		d := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: AppNS, Name: wl.Name},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(wl.Baseline), Selector: sel, Template: wl.template()},
		}
		if err := w.store.Create(ctx, d); err != nil {
			return err
		}
		wl.uid = string(d.UID)
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: AppNS, Name: wl.Name + "-" + podTemplateHash,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: wl.Name,
					UID: d.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}},
			},
			Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(wl.Baseline), Selector: sel, Template: wl.template()},
		}
		if err := w.store.Create(ctx, rs); err != nil {
			return err
		}
		wl.rsName, wl.rsUID = rs.Name, string(rs.UID)
	case v1alpha1.WorkloadKindStatefulSet:
		s := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: AppNS, Name: wl.Name},
			Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(wl.Baseline), Selector: sel, Template: wl.template(),
				ServiceName: wl.Name},
		}
		if err := w.store.Create(ctx, s); err != nil {
			return err
		}
		wl.uid = string(s.UID)
	case "":
		return wl.createPod(ctx, wl.Name)
	default:
		return fmt.Errorf("unsupported workload kind %q", wl.Kind)
	}
	return nil
}

// Replicas reads spec.replicas (1 for a bare pod).
func (wl *Workload) Replicas() int32 {
	if wl.Kind == "" {
		return 1
	}
	r, _ := wl.w.workloadReplicas(wl.Kind, AppNS, wl.Name)
	return r
}

// Pods returns the workload's pods (incl. terminating), sorted by name.
func (wl *Workload) Pods() []corev1.Pod {
	var list corev1.PodList
	if err := wl.w.store.List(wl.w.ctx, &list, client.InNamespace(AppNS), client.MatchingLabels(wl.labels())); err != nil {
		wl.w.violation("list pods of %s: %v", wl.Name, err)
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

// step is the replica controller.
func (wl *Workload) step() {
	if wl.Kind == "" {
		return
	}
	w := wl.w
	ctx := w.ctx
	desired, ok := w.workloadReplicas(wl.Kind, AppNS, wl.Name)
	if !ok {
		return
	}
	pods := wl.Pods()
	var active []corev1.Pod
	for _, p := range pods {
		if p.DeletionTimestamp == nil {
			active = append(active, p)
		}
	}
	switch wl.Kind {
	case v1alpha1.WorkloadKindDeployment:
		// Newest first out.
		sort.Slice(active, func(i, j int) bool { return podSeqOf(active[i].Name) < podSeqOf(active[j].Name) })
		for int32(len(active)) > desired {
			p := active[len(active)-1]
			active = active[:len(active)-1]
			wl.deletePod(&p)
		}
		for i := int32(len(active)); i < desired; i++ {
			wl.podSeq++
			if err := wl.createPod(ctx, fmt.Sprintf("%s-%s-%03d", wl.Name, podTemplateHash, wl.podSeq)); err != nil {
				w.violation("create pod: %v", err)
			}
		}
	case v1alpha1.WorkloadKindStatefulSet:
		names := map[string]bool{}
		for _, p := range pods {
			names[p.Name] = true
		}
		for _, p := range active {
			if ord, err := strconv.Atoi(strings.TrimPrefix(p.Name, wl.Name+"-")); err == nil && int32(ord) >= desired {
				wl.deletePod(&p)
			}
		}
		for i := int32(0); i < desired; i++ {
			name := fmt.Sprintf("%s-%d", wl.Name, i)
			if names[name] {
				continue
			}
			if err := wl.createPod(ctx, name); err != nil {
				w.violation("create pod: %v", err)
			}
		}
	}
	wl.updateStatus(pods)
}

func podSeqOf(name string) int {
	i := strings.LastIndex(name, "-")
	n, _ := strconv.Atoi(name[i+1:])
	return n
}

func (wl *Workload) createPod(ctx context.Context, name string) error {
	w := wl.w
	t := wl.template()
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: AppNS, Name: name, Labels: t.Labels, Finalizers: []string{podFinalizer}},
		Spec:       t.Spec,
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	p.Labels = map[string]string{"app": wl.Name}
	switch wl.Kind {
	case v1alpha1.WorkloadKindDeployment:
		p.Labels["pod-template-hash"] = podTemplateHash
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: wl.rsName,
			UID: types.UID(wl.rsUID), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}
	case v1alpha1.WorkloadKindStatefulSet:
		p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: wl.Name,
			UID: types.UID(wl.uid), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}
	}
	p.Spec.NodeName = wl.placement()
	if err := w.store.Create(ctx, p); err != nil {
		return err
	}
	w.podNames[p.UID] = p.Namespace + "/" + p.Name
	w.trace.add(Event{Kind: "pod", Obj: AppNS + "/" + name, Msg: "created on " + p.Spec.NodeName})
	return nil
}

// placement picks the node of Nodes with the fewest live pods of the workload.
func (wl *Workload) placement() string {
	count := map[string]int{}
	for _, p := range wl.Pods() {
		if p.DeletionTimestamp == nil {
			count[p.Spec.NodeName]++
		}
	}
	best := wl.Nodes[0]
	for _, n := range wl.Nodes[1:] {
		if count[n] < count[best] {
			best = n
		}
	}
	return best
}

func (wl *Workload) deletePod(p *corev1.Pod) {
	w := wl.w
	if err := w.store.Delete(w.ctx, p); err != nil {
		w.violation("delete pod %s: %v", p.Name, err)
		return
	}
	w.trace.add(Event{Kind: "pod", Obj: p.Namespace + "/" + p.Name, Msg: "deleting"})
}

// DeletePod deletes the workload's first live pod (a human or chaos).
func (wl *Workload) DeletePod() {
	for _, p := range wl.Pods() {
		if p.DeletionTimestamp == nil {
			wl.deletePod(&p)
			return
		}
	}
}

// updateStatus writes ready/total replicas from pods (this tick's list,
// before the controller's own creates and deletes: one tick of lag).
func (wl *Workload) updateStatus(pods []corev1.Pod) {
	w := wl.w
	var ready, total int32
	for _, p := range pods {
		if p.DeletionTimestamp == nil {
			total++
			if p.Status.Phase == corev1.PodRunning && podReady(&p) {
				ready++
			}
		}
	}
	switch wl.Kind {
	case v1alpha1.WorkloadKindDeployment:
		var d appsv1.Deployment
		if err := w.store.Get(w.ctx, client.ObjectKey{Namespace: AppNS, Name: wl.Name}, &d); err != nil {
			return
		}
		if d.Status.ReadyReplicas == ready && d.Status.Replicas == total {
			return
		}
		d.Status.ReadyReplicas, d.Status.Replicas, d.Status.AvailableReplicas = ready, total, ready
		if err := w.store.Status().Update(w.ctx, &d); err != nil {
			w.violation("deployment status: %v", err)
		}
	case v1alpha1.WorkloadKindStatefulSet:
		var s appsv1.StatefulSet
		if err := w.store.Get(w.ctx, client.ObjectKey{Namespace: AppNS, Name: wl.Name}, &s); err != nil {
			return
		}
		if s.Status.ReadyReplicas == ready && s.Status.Replicas == total {
			return
		}
		s.Status.ReadyReplicas, s.Status.Replicas, s.Status.AvailableReplicas = ready, total, ready
		if err := w.store.Status().Update(w.ctx, &s); err != nil {
			w.violation("statefulset status: %v", err)
		}
	}
}

// SetReplicas scales the workload as a human (or Flux) would. human=true
// also moves the baseline the invariants check against.
func (wl *Workload) SetReplicas(n int32, who string, human bool) {
	w := wl.w
	var obj client.Object
	switch wl.Kind {
	case v1alpha1.WorkloadKindDeployment:
		d := &appsv1.Deployment{}
		if err := w.store.Get(w.ctx, client.ObjectKey{Namespace: AppNS, Name: wl.Name}, d); err != nil {
			w.violation("get %s: %v", wl.Name, err)
			return
		}
		if ptr.Deref(d.Spec.Replicas, 1) == n {
			return
		}
		d.Spec.Replicas = ptr.To(n)
		obj = d
	case v1alpha1.WorkloadKindStatefulSet:
		s := &appsv1.StatefulSet{}
		if err := w.store.Get(w.ctx, client.ObjectKey{Namespace: AppNS, Name: wl.Name}, s); err != nil {
			w.violation("get %s: %v", wl.Name, err)
			return
		}
		if ptr.Deref(s.Spec.Replicas, 1) == n {
			return
		}
		s.Spec.Replicas = ptr.To(n)
		obj = s
	default:
		return
	}
	if err := w.store.Update(w.ctx, obj); err != nil {
		w.violation("scale %s: %v", wl.Name, err)
		return
	}
	if human {
		wl.Baseline = n
	}
	w.trace.add(Event{Kind: "scale", Obj: wl.ref(), To: strconv.Itoa(int(n)), Reason: who})
}

// Fighter is a Flux-like loop that keeps setting a workload back to its
// replicas while active.
type Fighter struct {
	wl       *Workload
	replicas int32
	every    time.Duration
	next     time.Time
	Active   bool
}

// Start activates the fighter; its first rescale happens after delay.
func (f *Fighter) Start(delay time.Duration) {
	f.Active = true
	f.next = f.wl.w.Now().Add(delay)
}

// AddFighter adds an inactive fighter for wl.
func (w *World) AddFighter(wl *Workload, replicas int32, every time.Duration) *Fighter {
	f := &Fighter{wl: wl, replicas: replicas, every: every}
	w.fighters = append(w.fighters, f)
	return f
}

func (f *Fighter) step() {
	w := f.wl.w
	if !f.Active || w.Now().Before(f.next) {
		return
	}
	f.next = w.Now().Add(f.every)
	if f.wl.Replicas() != f.replicas {
		f.wl.SetReplicas(f.replicas, "flux-fighter", false)
	}
}
