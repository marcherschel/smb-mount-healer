//go:build envtest

package controlplane_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/controlplane"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// ---------------------------------------------------------------- control plane

var (
	seq       atomic.Int64
	setLogger sync.Once
)

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano()%1000000, seq.Add(1))
}

// cpConfig returns a control-plane configuration with test timings.
func cpConfig(driverNS string) config.Config {
	return config.Config{
		Mode:   config.ModeControlPlane,
		Common: config.Common{MetricsBindAddress: "0", HealthProbeBindAddress: "0", CSIDriverName: "smb.csi.k8s.io"},
		ControlPlane: config.ControlPlane{
			CSINodeDriverNamespace:      driverNS,
			CSINodeDriverLabelSelector:  "app=csi-smb-node",
			LeaderElectionID:            uniq("lease"),
			PollInterval:                200 * time.Millisecond,
			PlanningProbeWait:           2 * time.Second,
			ScaleDownWait:               10 * time.Second,
			UnstageWait:                 time.Second,
			QuarantineUnstageWait:       5 * time.Second,
			DriverRestartWait:           5 * time.Second,
			VerifyWait:                  15 * time.Second,
			VerifyNoPodGrace:            5 * time.Second,
			EpisodeMaxDuration:          3 * time.Minute,
			BackoffBase:                 time.Minute,
			BackoffMax:                  10 * time.Minute,
			RemediationRetention:        24 * time.Hour,
			RemediationRetainMinPerNode: 10,
		},
	}
}

type cp struct {
	mgr  manager.Manager
	eng  *controlplane.Engine
	m    *metrics.ControlPlane
	stop func()
}

// startCP starts a control-plane manager against envtest; stop (also run
// at cleanup) cancels it and waits for it to return.
func startCP(t *testing.T, cfg config.Config, opts controlplane.Options) *cp {
	t.Helper()
	setLogger.Do(func() {
		out := io.Discard
		if os.Getenv("CP_TEST_LOG") != "" {
			ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
			return
		}
		ctrl.SetLogger(zap.New(zap.WriteTo(out), zap.UseDevMode(true)))
	})
	opts.RESTConfig = restConfig
	opts.Registry = prometheus.NewRegistry()
	opts.Metrics = metrics.NewControlPlane()
	opts.SkipControllerNameValidation = true
	mgr, eng, err := controlplane.NewManager(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctrl.LoggerInto(context.Background(), ctrl.Log.WithName(t.Name())))
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Logf("manager stopped: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Error("manager did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return &cp{mgr: mgr, eng: eng, m: opts.Metrics, stop: stop}
}

// metricValue returns the value of a single counter or gauge.
func metricValue(c prometheus.Metric) float64 {
	var d dto.Metric
	if err := c.Write(&d); err != nil {
		panic(err)
	}
	if d.Counter != nil {
		return d.Counter.GetValue()
	}
	return d.GetGauge().GetValue()
}

// ---------------------------------------------------------------- fake world

// world emulates everything around the control plane on one PV shared by
// one or more nodes: a StatefulSet "app" (ordinal i runs on nodes[i%n])
// with a replica controller, the CSI driver pods, the worker (SmbMount
// status, SmbHostTask execution) and kubelet's staging.
type world struct {
	t        *testing.T
	ctx      context.Context
	ns       string
	driverNS string
	pv       string
	nodes    []string

	mu      sync.Mutex
	gm      map[string]*gmState // per node
	broken  map[types.UID]bool  // pods whose mount is broken
	applied map[string]string   // SmbMount -> last applied status JSON
	driverN int
	ltt     metav1.Time
	hold    atomic.Bool // host tasks are not executed
	noDrain atomic.Bool // scaling down does not remove pods
	// outage: the share is unreachable. kubelet cannot stage, new pods stay
	// Pending, and once nothing of the volume is left on a node the worker
	// deletes its SmbMount (as the real worker does).
	outage   atomic.Bool
	stopLoop func()
}

type gmState struct{ mounted, dirExists, broken bool }

func gmPath(pv string) string {
	return "/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/" + pv + "-hash/globalmount"
}

func newWorld(t *testing.T, replicas int32, nodes ...string) *world {
	t.Helper()
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cp-test-"}}
	g.Expect(k8s.Create(ctx, nsObj)).To(Succeed())
	ns := nsObj.Name
	drv := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cp-driver-"}}
	g.Expect(k8s.Create(ctx, drv)).To(Succeed())
	w := &world{
		t: t, ctx: ctx, ns: ns, driverNS: drv.Name, pv: uniq("pv"), nodes: nodes,
		gm: map[string]*gmState{}, broken: map[types.UID]bool{}, applied: map[string]string{},
		ltt: metav1.NewTime(time.Now().UTC().Truncate(time.Second)),
	}
	g.Expect(k8s.Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
			VolumeName:  w.pv, StorageClassName: ptr.To(""),
		},
	})).To(Succeed())
	l := map[string]string{"app": "app"}
	g.Expect(k8s.Create(ctx, &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas), Selector: &metav1.LabelSelector{MatchLabels: l}, ServiceName: "app",
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l}, Spec: podSpec("")},
		},
	})).To(Succeed())
	for _, n := range nodes {
		w.gm[n] = &gmState{mounted: true, dirExists: true}
		w.newDriverPod(n)
		g.Expect(k8s.Create(ctx, &v1.SmbMount{
			ObjectMeta: metav1.ObjectMeta{Name: v1.SmbMountName(n, w.pv), Labels: map[string]string{v1.LabelNode: n, v1.LabelPV: w.pv}},
			Spec:       v1.SmbMountSpec{NodeName: n, PVName: w.pv, Share: "//samba/" + w.pv, VolumeHandle: w.pv, GlobalmountPath: gmPath(w.pv)},
		})).To(Succeed())
	}
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			w.tick()
			select {
			case <-loopCtx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	w.stopLoop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(w.cleanup)
	t.Cleanup(w.stopLoop)
	return w
}

func podSpec(node string) corev1.PodSpec {
	return corev1.PodSpec{
		NodeName:   node,
		Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"},
		}}},
	}
}

func (w *world) logf(format string, a ...any) {
	if os.Getenv("CP_TEST_LOG") != "" {
		w.t.Logf(format, a...)
	}
}

func (w *world) newDriverPod(node string) {
	w.driverN++
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: w.driverNS, Name: fmt.Sprintf("csi-smb-node-%s-%d", node, w.driverN),
			Labels: map[string]string{"app": "csi-smb-node"}},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "smb", Image: "csi"}}},
	}
	if err := k8s.Create(w.ctx, p); err != nil {
		w.t.Errorf("create driver pod: %v", err)
		return
	}
	w.markRunning(p)
}

func (w *world) markRunning(p *corev1.Pod) {
	p.Status.Phase = corev1.PodRunning
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := k8s.Status().Update(w.ctx, p); err != nil && w.ctx.Err() == nil {
		w.logf("mark pod running: %v", err)
	}
}

// pods returns the app pods (any node), keyed by name.
func (w *world) pods() map[string]corev1.Pod {
	var list corev1.PodList
	if err := k8s.List(w.ctx, &list, client.InNamespace(w.ns)); err != nil {
		return nil
	}
	out := map[string]corev1.Pod{}
	for _, p := range list.Items {
		out[p.Name] = p
	}
	return out
}

func (w *world) tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx.Err() != nil {
		return
	}
	w.replicaController()
	w.driverController()
	w.worker()
	w.kubelet()
	w.publish()
}

func (w *world) replicaController() {
	var sts appsv1.StatefulSet
	if err := k8s.Get(w.ctx, client.ObjectKey{Namespace: w.ns, Name: "app"}, &sts); err != nil {
		return
	}
	desired := int(ptr.Deref(sts.Spec.Replicas, 1))
	pods := w.pods()
	for name, p := range pods {
		ord, _ := strconv.Atoi(strings.TrimPrefix(name, "app-"))
		if p.DeletionTimestamp != nil || (ord >= desired && !w.noDrain.Load()) {
			_ = k8s.Delete(w.ctx, &p, client.GracePeriodSeconds(0))
			delete(pods, name)
		}
	}
	for i := 0; i < desired; i++ {
		name := fmt.Sprintf("app-%d", i)
		if _, ok := pods[name]; ok {
			continue
		}
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: w.ns, Name: name, Labels: map[string]string{"app": "app"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name,
					UID: sts.UID, Controller: ptr.To(true)}}},
			Spec: podSpec(w.nodes[i%len(w.nodes)]),
		}
		if err := k8s.Create(w.ctx, p); err != nil {
			continue
		}
		if w.outage.Load() {
			w.logf("world: created pod %s uid %s (Pending: share unreachable)", name, p.UID)
			continue
		}
		w.markRunning(p)
		w.logf("world: created pod %s uid %s", name, p.UID)
	}
}

func (w *world) driverController() {
	var list corev1.PodList
	if err := k8s.List(w.ctx, &list, client.InNamespace(w.driverNS)); err != nil {
		return
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.DeletionTimestamp == nil {
			continue
		}
		if err := k8s.Delete(w.ctx, p, client.GracePeriodSeconds(0)); err == nil {
			w.logf("world: driver pod %s restarted", p.Name)
			w.newDriverPod(p.Spec.NodeName)
		}
	}
}

func (w *world) node(n string) bool {
	for _, x := range w.nodes {
		if x == n {
			return true
		}
	}
	return false
}

// worker executes host tasks against the emulated host state.
func (w *world) worker() {
	if w.hold.Load() {
		return
	}
	var list v1.SmbHostTaskList
	if err := k8s.List(w.ctx, &list); err != nil {
		return
	}
	for i := range list.Items {
		task := &list.Items[i]
		if !w.node(task.Spec.NodeName) || task.Status.Phase != "" {
			continue
		}
		gm := w.gm[task.Spec.NodeName]
		now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
		for _, a := range task.Spec.Actions {
			res := v1.HostActionResult{ID: a.ID, Result: v1.HostActionSucceeded, Time: now}
			switch {
			case task.Spec.DryRun:
				res.Result = v1.HostActionDryRun
			case a.Type == v1.HostActionForceUnmount && a.Path == gmPath(w.pv):
				gm.mounted = false
			case a.Type == v1.HostActionQuarantine && a.Path == gmPath(w.pv) && !gm.mounted:
				gm.dirExists = false
			}
			task.Status.Results = append(task.Status.Results, res)
		}
		task.Status.Phase = v1.HostTaskSucceeded
		task.Status.ObservedGeneration = task.Generation
		if err := k8s.Status().Update(w.ctx, task); err == nil {
			w.logf("world: task %s done", task.Name)
		}
	}
}

// kubelet re-stages a globalmount whose dir is gone once a pod needs it.
func (w *world) kubelet() {
	if w.outage.Load() {
		return
	}
	pods := w.pods()
	for n, gm := range w.gm {
		if gm.mounted || gm.dirExists {
			continue
		}
		for _, p := range pods {
			if p.Spec.NodeName == n && p.DeletionTimestamp == nil {
				*gm = gmState{mounted: true, dirExists: true}
				w.logf("world: %s re-staged", n)
				break
			}
		}
	}
}

// publish server-side-applies the worker's SmbMount status fields.
func (w *world) publish() {
	pods := w.pods()
	names := make([]string, 0, len(pods))
	for n := range pods {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range w.nodes {
		gm := w.gm[n]
		if w.outage.Load() && !gm.mounted && !gm.dirExists {
			// No globalmount and no pod mount left: the worker deletes the SmbMount.
			name := v1.SmbMountName(n, w.pv)
			if _, ok := w.applied[name]; ok {
				if err := k8s.Delete(w.ctx, &v1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: name}}); err == nil || apierrors.IsNotFound(err) {
					delete(w.applied, name)
					w.logf("world: SmbMount %s deleted (nothing left on the node)", name)
				}
			}
			continue
		}
		health, reason, fstype := v1.HealthUnknown, v1.FailureReason(""), ""
		if gm.mounted {
			health, fstype = v1.HealthHealthy, "cifs"
			if gm.broken {
				health, reason, fstype = v1.HealthBroken, v1.ReasonWrongFSType, "xfs"
			}
		}
		st := ac.SmbMountStatus().WithHealth(health).
			WithGlobalmount(ac.GlobalmountStatus().WithMounted(gm.mounted).WithFSType(fstype).WithDirExists(gm.dirExists).WithEmpty(true))
		if reason != "" {
			st.WithReason(reason)
		}
		for _, name := range names {
			p := pods[name]
			if p.Spec.NodeName != n || p.DeletionTimestamp != nil {
				continue
			}
			pm := ac.PodMountStatus().WithNamespace(p.Namespace).WithPod(p.Name).WithUID(p.UID).WithMounted(gm.mounted).
				WithHealth(v1.HealthHealthy).WithFSType("cifs")
			if w.broken[p.UID] {
				pm.WithHealth(v1.HealthBroken).WithReason(v1.ReasonWrongFSType).WithFSType("xfs")
			}
			st.WithPodMounts(pm)
		}
		cond := metav1.ConditionFalse
		if health == v1.HealthHealthy {
			cond = metav1.ConditionTrue
		}
		st.WithConditions(metav1ac.Condition().WithType(v1.ConditionHealthy).WithStatus(cond).WithReason("Probe").
			WithMessage("probe").WithLastTransitionTime(w.ltt))
		name := v1.SmbMountName(n, w.pv)
		b, _ := json.Marshal(st)
		if w.applied[name] == string(b) {
			continue
		}
		err := k8s.Status().Apply(w.ctx, ac.SmbMount(name).WithStatus(st), client.FieldOwner(v1.FieldManagerWorker), client.ForceOwnership)
		if err == nil {
			w.applied[name] = string(b)
		}
	}
}

// waitPods waits until the replica controller created n pods.
func (w *world) waitPods(n int) {
	w.t.Helper()
	NewWithT(w.t).Eventually(func() int { return len(w.pods()) }).WithTimeout(20 * time.Second).Should(Equal(n))
}

// breakNode makes node's globalmount and every current pod mount on it Broken.
func (w *world) breakNode(node string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.gm[node].broken = true
	for _, p := range w.pods() {
		if p.Spec.NodeName == node {
			w.broken[p.UID] = true
		}
	}
}

func (w *world) replicas() int32 {
	var sts appsv1.StatefulSet
	if err := k8s.Get(w.ctx, client.ObjectKey{Namespace: w.ns, Name: "app"}, &sts); err != nil {
		return -1
	}
	return ptr.Deref(sts.Spec.Replicas, 1)
}

func (w *world) smbMount(node string) v1.SmbMount {
	var sm v1.SmbMount
	_ = k8s.Get(w.ctx, client.ObjectKey{Name: v1.SmbMountName(node, w.pv)}, &sm)
	return sm
}

// remediations returns the remediations of node, oldest first.
func remediations(ctx context.Context, node string) []v1.SmbMountRemediation {
	var list v1.SmbMountRemediationList
	if err := k8s.List(ctx, &list); err != nil {
		return nil
	}
	var out []v1.SmbMountRemediation
	for _, r := range list.Items {
		if r.Spec.NodeName == node {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// cleanup removes the cluster-scoped objects of the world's nodes so later
// tests' control planes never see them.
func (w *world) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range w.nodes {
		for _, r := range remediations(ctx, n) {
			if controllerutil.RemoveFinalizer(&r, v1.FinalizerRestoreReplicas) {
				_ = k8s.Update(ctx, &r)
			}
			_ = k8s.Delete(ctx, &r)
		}
		_ = k8s.DeleteAllOf(ctx, &v1.SmbHostTask{}, client.MatchingLabels{v1.LabelNode: n})
		_ = k8s.Delete(ctx, &v1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: v1.SmbMountName(n, w.pv)}})
	}
}

// stages returns the stage path of r from its transitions.
func stages(r *v1.SmbMountRemediation) []v1.Stage {
	var out []v1.Stage
	for _, t := range r.Status.Transitions {
		out = append(out, t.To)
	}
	return out
}
