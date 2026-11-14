package sim

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoapplyconfigurations "k8s.io/client-go/applyconfigurations"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration"
	"github.com/marcherschel/smb-mount-healer/internal/config"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

// Fixed layout of the simulated cluster.
const (
	KubeletRoot   = "/var/lib/kubelet"
	DriverName    = "smb.csi.k8s.io"
	QuarantineDir = KubeletRoot + "/smb-mount-healer-quarantine"
	DriverNS      = "kube-system"
	AppNS         = "apps"
	// SambaServer is the default samba server; SambaLandmine serves the
	// dead-orphan scenario.
	SambaServer   = "samba.e2e.svc.cluster.local"
	SambaLandmine = "samba-landmine.e2e.svc.cluster.local"

	// Tick is the fixed simulation step.
	Tick = time.Second

	podFinalizer = "sim.smbhealer.herschel.io/kubelet"
	driverLabel  = "csi-smb-node"
	// SecretPrefix marks every file name written to local disk (shadow
	// data). No string with this prefix may ever appear in a CR, an Event
	// or a log line.
	SecretPrefix = "SECRET-"
)

// Epoch is the simulated start time.
var Epoch = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)

// Options configures a World.
type Options struct {
	// Nodes are the node names (default ["n1"]).
	Nodes []string
	// DryRun runs the control plane with --dry-run.
	DryRun bool
	// ControlPlane and Worker may adjust the default (production) timings.
	ControlPlane func(*config.ControlPlane)
	Worker       func(*config.Worker)
}

// PV is one simulated smb PersistentVolume with its PVC (same name, AppNS).
type PV struct {
	Name   string
	Server string
	Share  string
	Source string // //server/share
	Handle string
	GMPath string
}

// World is the simulated cluster: one fake API server, nodes with a fake
// kubelet, hostfs and detached worker each, a fake samba, a CSI driver
// DaemonSet, a replica emulator and one control-plane Engine, all driven by
// a fake clock in fixed ticks.
type World struct {
	ctx    context.Context
	Clock  *clocktesting.FakeClock
	scheme *runtime.Scheme
	store  client.WithWatch // fake API with UID/creationTimestamp/precondition semantics
	cpc    client.WithWatch // control-plane client (crash interceptor)
	layout kubelet.Layout
	cpCfg  config.ControlPlane
	wkCfg  config.Worker
	dryRun bool

	Nodes      []*Node
	nodeByName map[string]*Node
	Samba      *Samba
	pvs        map[string]*PV
	pvOrder    []string
	workloads  []*Workload
	fighters   []*Fighter
	cp         *ControlPlane

	cpMetrics *metrics.ControlPlane
	reg       *prometheus.Registry
	trace     *Trace
	obs       *observer
	hooks     []*hook
	secrets   map[string]bool
	podNames  map[types.UID]string
	log       logr.Logger
	uidSeq    int

	violations     []string
	dryRunAttempts int
	// remsMemo caches the remediation list while hooks are evaluated.
	remsMemo *[]v1alpha1.SmbMountRemediation
	started  bool
	// crash points (see crash.go)
	cpCrash     *cpCrashPoint
	workerCrash *workerCrashPoint
}

// NewWorld returns an empty world. Add fixtures, then Start it.
func NewWorld(opts Options) (*World, error) {
	if len(opts.Nodes) == 0 {
		opts.Nodes = []string{"n1"}
	}
	scheme, err := kube.NewScheme()
	if err != nil {
		return nil, err
	}
	w := &World{
		ctx:        context.Background(),
		Clock:      clocktesting.NewFakeClock(Epoch),
		scheme:     scheme,
		layout:     kubelet.Layout{Root: KubeletRoot, DriverName: DriverName},
		dryRun:     opts.DryRun,
		nodeByName: map[string]*Node{},
		pvs:        map[string]*PV{},
		reg:        prometheus.NewRegistry(),
		secrets:    map[string]bool{},
		podNames:   map[types.UID]string{},
	}
	w.Samba = newSamba(w)
	w.trace = newTrace(w)
	w.obs = newObserver(w)
	w.log = funcr.New(w.logSink, funcr.Options{Verbosity: 1})
	w.ctx = logr.NewContext(w.ctx, w.log)
	w.cpCfg = config.ControlPlane{
		DryRun:                      opts.DryRun,
		CSINodeDriverNamespace:      DriverNS,
		CSINodeDriverLabelSelector:  "app=" + driverLabel,
		PollInterval:                3 * time.Second,
		PlanningProbeWait:           15 * time.Second,
		ScaleDownWait:               120 * time.Second,
		UnstageWait:                 30 * time.Second,
		QuarantineUnstageWait:       300 * time.Second,
		DriverRestartWait:           90 * time.Second,
		VerifyWait:                  90 * time.Second,
		VerifyNoPodGrace:            30 * time.Second,
		EpisodeMaxDuration:          30 * time.Minute,
		BackoffBase:                 5 * time.Minute,
		BackoffMax:                  time.Hour,
		RemediationRetention:        24 * time.Hour,
		RemediationRetainMinPerNode: 10,
	}
	if opts.ControlPlane != nil {
		opts.ControlPlane(&w.cpCfg)
	}
	w.wkCfg = config.Worker{
		KubeletRoot:              KubeletRoot,
		ExpectedFSTypes:          []string{"cifs", "smb3"},
		CheckInterval:            60 * time.Second,
		RecheckAttempts:          3,
		RecheckDelay:             3 * time.Second,
		StatTimeout:              5 * time.Second,
		StartupGrace:             10 * time.Second,
		PodStartupGrace:          60 * time.Second,
		SlowConfirmWindow:        3 * time.Minute,
		SlowRecoveryWindow:       2 * time.Minute,
		ProbeConcurrency:         1,
		StatusHeartbeat:          5 * time.Minute,
		QuarantineDir:            QuarantineDir,
		QuarantineScanInterval:   5 * time.Minute,
		QuarantineScanMaxEntries: 100000,
		QuarantineScanTimeBudget: 10 * time.Second,
	}
	if opts.Worker != nil {
		opts.Worker(&w.wkCfg)
	}

	cgs := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(cgs); err != nil {
		return nil, err
	}
	fc := clientfake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.SmbMount{}, &v1alpha1.SmbQuarantine{}, &v1alpha1.SmbHostTask{},
			&v1alpha1.SmbMountRemediation{}).
		WithIndex(&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string {
			if p, ok := o.(*corev1.Pod); ok {
				return []string{p.Spec.NodeName}
			}
			return nil
		}).
		WithTypeConverters(applyconfiguration.NewTypeConverter(scheme),
			clientgoapplyconfigurations.NewTypeConverter(cgs), managedfields.NewDeducedTypeConverter()).
		Build()
	w.store = interceptor.NewClient(fc, interceptor.Funcs{Create: w.icCreate, Delete: w.icDelete})
	w.cpc = interceptor.NewClient(w.store, interceptor.Funcs{SubResourceUpdate: w.icCPStatusUpdate, Create: w.icCPCreate})

	w.cpMetrics = metrics.NewControlPlane()
	if err := w.cpMetrics.Register(w.reg); err != nil {
		return nil, err
	}
	w.cpMetrics.SetDryRun(opts.DryRun)
	for _, name := range opts.Nodes {
		n, err := newNode(w, name)
		if err != nil {
			return nil, err
		}
		w.Nodes = append(w.Nodes, n)
		w.nodeByName[name] = n
	}
	w.cp = &ControlPlane{w: w}
	return w, nil
}

// Now is the simulated time.
func (w *World) Now() time.Time { return w.Clock.Now() }

// Elapsed is the simulated time since Epoch.
func (w *World) Elapsed() time.Duration { return w.Now().Sub(Epoch) }

// Client is the shared fake API client (no crash interceptors).
func (w *World) Client() client.Client { return w.store }

// Ctx returns the world's context (carries the capturing logger).
func (w *World) Ctx() context.Context { return w.ctx }

// Node returns a node by name.
func (w *World) Node(name string) *Node { return w.nodeByName[name] }

// Layout is the kubelet layout of every node.
func (w *World) Layout() kubelet.Layout { return w.layout }

// PVByName returns a PV fixture.
func (w *World) PVByName(name string) *PV { return w.pvs[name] }

// --- fake API semantics -------------------------------------------------

// icCreate sets a deterministic UID and the creationTimestamp (the fake
// client sets neither).
func (w *World) icCreate(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetUID() == "" {
		w.uidSeq++
		obj.SetUID(types.UID(fmt.Sprintf("uid-%05d", w.uidSeq)))
	}
	if ts := obj.GetCreationTimestamp(); ts.IsZero() {
		obj.SetCreationTimestamp(metav1.NewTime(w.Now()))
	}
	return c.Create(ctx, obj, opts...)
}

// icDelete enforces the UID precondition, which the fake client ignores.
func (w *World) icDelete(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
	var do client.DeleteOptions
	do.ApplyOptions(opts)
	if do.Preconditions != nil && do.Preconditions.UID != nil {
		cur, ok := obj.DeepCopyObject().(client.Object)
		if !ok {
			return errors.New("sim: not a client.Object")
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), cur); err != nil {
			return err
		}
		if cur.GetUID() != *do.Preconditions.UID {
			gvk, _ := c.GroupVersionKindFor(obj)
			return apierrors.NewConflict(
				schema.GroupResource{Group: gvk.Group, Resource: strings.ToLower(gvk.Kind) + "s"}, obj.GetName(),
				fmt.Errorf("precondition failed: UID in precondition: %s, UID in object meta: %s", *do.Preconditions.UID, cur.GetUID()))
		}
	}
	return c.Delete(ctx, obj, opts...)
}

// --- fixtures -----------------------------------------------------------

// AddPV adds an smb PV (volumeHandle = name) on server/share with a bound
// PVC of the same name in AppNS.
func (w *World) AddPV(name, server, share string) *PV {
	pv := &PV{Name: name, Server: server, Share: share, Source: "//" + server + "/" + share, Handle: name,
		GMPath: w.layout.GlobalmountPath(name)}
	w.pvs[name] = pv
	w.pvOrder = append(w.pvOrder, name)
	return pv
}

// Start creates the cluster objects and runs the warm-up (untraced) until
// every workload runs with healthy mounts, then starts tracing.
func (w *World) Start(warmup time.Duration) error {
	ctx := w.ctx
	for _, n := range w.Nodes {
		if err := w.store.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name}}); err != nil {
			return err
		}
	}
	for _, name := range w.pvOrder {
		pv := w.pvs[name]
		obj := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: pv.Name},
			Spec: corev1.PersistentVolumeSpec{
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				MountOptions:                  []string{"dir_mode=0777", "file_mode=0777", "vers=3.0"},
				ClaimRef:                      &corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: AppNS, Name: pv.Name},
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
					Driver: DriverName, VolumeHandle: pv.Handle, VolumeAttributes: map[string]string{"source": pv.Source},
				}},
			},
		}
		if err := w.store.Create(ctx, obj); err != nil {
			return err
		}
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: AppNS, Name: pv.Name},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				VolumeName:  pv.Name, StorageClassName: ptr.To("smb"),
			},
		}
		if err := w.store.Create(ctx, pvc); err != nil {
			return err
		}
	}
	for _, wl := range w.workloads {
		if err := wl.create(ctx); err != nil {
			return err
		}
	}
	for _, n := range w.Nodes {
		if err := n.startWorker(false); err != nil {
			return err
		}
	}
	if err := w.cp.start(false); err != nil {
		return err
	}
	w.started = true
	w.trace.enabled = false
	for w.Elapsed() < warmup {
		w.Step()
	}
	w.trace.enabled = true
	w.obs.resetQuiet()
	w.trace.add(Event{Kind: "baseline", Msg: w.baselineSummary()})
	return nil
}

func (w *World) baselineSummary() string {
	var parts []string
	for _, wl := range w.workloads {
		parts = append(parts, fmt.Sprintf("%s=%d", wl.ref(), wl.Baseline))
	}
	return strings.Join(parts, " ")
}

// Step advances the world by one Tick in a fixed order.
func (w *World) Step() {
	w.Clock.Step(Tick)
	w.runHooks()
	for _, n := range w.Nodes {
		n.stepDriver()
	}
	for _, wl := range w.workloads {
		wl.step()
	}
	for _, f := range w.fighters {
		f.step()
	}
	for _, n := range w.Nodes {
		n.kubelet.step()
	}
	for _, n := range w.Nodes {
		n.stepWorker()
	}
	w.stepGarbageCollector()
	w.cp.step()
	w.obs.observe()
	w.checkInvariants()
}

// RunFor steps the world for d.
func (w *World) RunFor(d time.Duration) {
	end := w.Now().Add(d)
	for w.Now().Before(end) {
		w.Step()
	}
}

// RunUntil steps until cond holds or limit elapsed; it reports whether cond held.
func (w *World) RunUntil(limit time.Duration, cond func() bool) bool {
	end := w.Now().Add(limit)
	for w.Now().Before(end) {
		if cond() {
			return true
		}
		w.Step()
	}
	return cond()
}

// stepGarbageCollector emulates ownerReference garbage collection for the
// healer's own kinds: SmbHostTasks of deleted remediations, and objects
// owned by deleted Nodes.
func (w *World) stepGarbageCollector() {
	ctx := w.ctx
	var rems v1alpha1.SmbMountRemediationList
	if err := w.store.List(ctx, &rems); err != nil {
		w.violation("gc: list remediations: %v", err)
		return
	}
	alive := map[types.UID]bool{}
	for _, r := range rems.Items {
		alive[r.UID] = true
	}
	var tasks v1alpha1.SmbHostTaskList
	if err := w.store.List(ctx, &tasks); err != nil {
		w.violation("gc: list tasks: %v", err)
		return
	}
	for i := range tasks.Items {
		t := &tasks.Items[i]
		for _, o := range t.OwnerReferences {
			if o.Kind == "SmbMountRemediation" && !alive[o.UID] {
				if err := w.store.Delete(ctx, t); err != nil && !apierrors.IsNotFound(err) {
					w.violation("gc: delete task %s: %v", t.Name, err)
				}
				break
			}
		}
	}
}

// logSink receives every log line of both modes. Lines are not traced
// (they are free text), only checked for quarantined names.
func (w *World) logSink(prefix, args string) {
	w.checkSecret("log", prefix+" "+args)
}

func (w *World) checkSecret(where, s string) {
	if !strings.Contains(s, SecretPrefix) {
		return
	}
	for name := range w.secrets {
		if strings.Contains(s, name) {
			w.violation("quarantined file name %q leaked into %s: %.300s", name, where, s)
			return
		}
	}
}

func (w *World) addSecret(name string) { w.secrets[name] = true }

func (w *World) violation(format string, args ...any) {
	msg := fmt.Sprintf("%s %s", fmtElapsed(w.Elapsed()), fmt.Sprintf(format, args...))
	if len(w.violations) < 50 {
		w.violations = append(w.violations, msg)
	}
}

// Violations returns the invariant violations recorded so far.
func (w *World) Violations() []string { return w.violations }

// --- helpers --------------------------------------------------------------

func (w *World) listPods() []corev1.Pod {
	var list corev1.PodList
	if err := w.store.List(w.ctx, &list); err != nil {
		w.violation("list pods: %v", err)
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := &list.Items[i], &list.Items[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return list.Items
}

func (w *World) remediations() []v1alpha1.SmbMountRemediation {
	if w.remsMemo != nil {
		return *w.remsMemo
	}
	var list v1alpha1.SmbMountRemediationList
	if err := w.store.List(w.ctx, &list); err != nil {
		w.violation("list remediations: %v", err)
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

func (w *World) smbMounts() []v1alpha1.SmbMount {
	var list v1alpha1.SmbMountList
	if err := w.store.List(w.ctx, &list); err != nil {
		w.violation("list SmbMounts: %v", err)
		return nil
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items
}

// pvOfClaim resolves a PVC in ns to its PV fixture.
func (w *World) pvOfClaim(ns, claim string) *PV {
	if ns != AppNS {
		return nil
	}
	return w.pvs[claim]
}

// podPVs returns the smb PVs of pod, sorted.
func (w *World) podPVs(p *corev1.Pod) []*PV {
	var out []*PV
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		if pv := w.pvOfClaim(p.Namespace, v.PersistentVolumeClaim.ClaimName); pv != nil {
			out = append(out, pv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func driverSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{"app": driverLabel})
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func setPodRunning(p *corev1.Pod, now time.Time) {
	t := metav1.NewTime(now)
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = &t
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: t}}
}

func fmtElapsed(d time.Duration) string {
	s := int64(d / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s/60)%60, s%60)
}

// workloadReplicas reads spec.replicas of a Deployment or StatefulSet.
func (w *World) workloadReplicas(kind v1alpha1.WorkloadKind, ns, name string) (int32, bool) {
	switch kind {
	case v1alpha1.WorkloadKindDeployment:
		var d appsv1.Deployment
		if err := w.store.Get(w.ctx, client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
			return 0, false
		}
		return ptr.Deref(d.Spec.Replicas, 1), true
	case v1alpha1.WorkloadKindStatefulSet:
		var s appsv1.StatefulSet
		if err := w.store.Get(w.ctx, client.ObjectKey{Namespace: ns, Name: name}, &s); err != nil {
			return 0, false
		}
		return ptr.Deref(s.Spec.Replicas, 1), true
	}
	return 0, false
}

func typesUID(s string) types.UID { return types.UID(s) }

func fakeDir() fake.Dir { return fake.Dir{} }

// Expectf records a problem unless cond holds (scenario assertions in hooks).
func (w *World) Expectf(cond bool, format string, args ...any) {
	if !cond {
		w.violation("expectation failed: "+format, args...)
	}
}
