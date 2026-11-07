package sim

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/controlplane"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
)

// gcEvery is how often the GC controller runs per node (its real requeue
// is bounded by an hour; remediation changes also enqueue it).
const gcEvery = time.Minute

// ControlPlane drives one controlplane.Engine (the leader). A crash or
// leader change replaces it with a fresh Engine on the same API state.
type ControlPlane struct {
	w      *World
	eng    *controlplane.Engine
	gen    int
	down   bool
	upAt   time.Time
	nextGC time.Time
	// standby is a pre-built second Engine for leader changes.
	standby *controlplane.Engine
}

func (cp *ControlPlane) newEngine() (*controlplane.Engine, error) {
	w := cp.w
	kc, err := kube.NewClientCluster(kube.ClientOptions{
		Client: w.cpc, APIReader: w.cpc, Recorder: &recorder{w: w, source: "controlplane"},
		DriverNamespace: DriverNS, DriverSelector: driverSelector(),
	})
	if err != nil {
		return nil, err
	}
	exec, err := controlplane.NewExecutor(controlplane.ExecutorOptions{
		Cluster: kc, Client: w.cpc, Metrics: w.cpMetrics, DryRun: w.cpCfg.DryRun, Clock: w.Clock,
	})
	if err != nil {
		return nil, err
	}
	return controlplane.NewEngine(controlplane.EngineOptions{
		Client: w.cpc, APIReader: w.cpc, Cluster: kc,
		Executor: &recordingExecutor{w: w, inner: exec},
		Metrics:  w.cpMetrics, Clock: w.Clock, Config: w.cpCfg,
	})
}

func (cp *ControlPlane) start(restart bool) error {
	eng, err := cp.newEngine()
	if err != nil {
		return err
	}
	cp.eng = eng
	cp.gen++
	cp.down = false
	cp.nextGC = cp.w.Now()
	if restart {
		cp.w.trace.add(Event{Kind: "controlplane", Msg: fmt.Sprintf("started (fresh process #%d)", cp.gen)})
	}
	return nil
}

// Crash kills the control plane; a fresh Engine takes over after downtime.
func (cp *ControlPlane) Crash(downtime time.Duration) {
	cp.eng = nil
	cp.down = true
	cp.upAt = cp.w.Now().Add(downtime)
	cp.w.trace.add(Event{Kind: "controlplane", Msg: fmt.Sprintf("crashed (down %s)", downtime)})
}

// PrepareStandby builds the second replica's Engine ahead of a leader change.
func (cp *ControlPlane) PrepareStandby() error {
	eng, err := cp.newEngine()
	cp.standby = eng
	return err
}

// LeaderChange hands leadership to the standby Engine (the old leader
// stopped reconciling before the new one acquired the Lease).
func (cp *ControlPlane) LeaderChange() {
	if cp.standby == nil {
		cp.w.violation("leader change without a standby")
		return
	}
	cp.eng, cp.standby = cp.standby, nil
	cp.gen++
	cp.w.trace.add(Event{Kind: "controlplane", Msg: fmt.Sprintf("leader changed to replica #%d", cp.gen)})
}

// Generation is the number of control-plane processes so far.
func (cp *ControlPlane) Generation() int { return cp.gen }

// step runs, in order: the trigger controller per node, the remediation
// reconciler per unfinished remediation, and (every gcEvery) the GC.
func (cp *ControlPlane) step() {
	w := cp.w
	if cp.down {
		if w.Now().Before(cp.upAt) {
			return
		}
		if err := cp.start(true); err != nil {
			w.violation("restart control plane: %v", err)
			return
		}
	}
	ctx := w.ctx
	for _, n := range w.Nodes {
		if cp.call(func(e *controlplane.Engine) (reconcile.Result, error) { return e.ReconcileTrigger(ctx, n.Name) }, "trigger/"+n.Name) {
			return
		}
	}
	for _, r := range w.remediations() {
		if r.Status.Stage.Terminal() && r.DeletionTimestamp == nil && len(r.Finalizers) == 0 {
			continue
		}
		if cp.call(func(e *controlplane.Engine) (reconcile.Result, error) { return e.ReconcileRemediation(ctx, r.Name) }, "remediation/"+r.Name) {
			return
		}
	}
	if !w.Now().Before(cp.nextGC) {
		cp.nextGC = w.Now().Add(gcEvery)
		for _, n := range w.Nodes {
			if cp.call(func(e *controlplane.Engine) (reconcile.Result, error) { return e.ReconcileGC(ctx, n.Name) }, "gc/"+n.Name) {
				return
			}
		}
	}
}

// call runs one reconcile; it reports whether the control plane crashed.
func (cp *ControlPlane) call(fn func(*controlplane.Engine) (reconcile.Result, error), what string) bool {
	if cp.eng == nil {
		return true
	}
	eng := cp.eng
	var err error
	crashed := protect(func() { _, err = fn(eng) })
	if crashed {
		cp.Crash(cp.w.cpCrashDowntime())
		return true
	}
	if err != nil {
		cp.w.trace.errorf("controlplane", "%s: %v", what, err)
	}
	return false
}

// recordingExecutor traces every cluster command that changes something,
// then delegates to the real controlplane.Executor. Mirror patches reach it
// only through the Engine's SmbMountStatusWriter.
type recordingExecutor struct {
	w     *World
	inner command.ClusterExecutor
}

func (r *recordingExecutor) ExecuteCluster(ctx context.Context, cmd command.ClusterCommand) error {
	w := r.w
	dry := ""
	if w.cpCfg.DryRun {
		dry = " (dry run: not executed)"
	}
	switch c := cmd.(type) {
	case command.ScaleWorkload:
		cur, ok := w.workloadReplicas(c.Workload.Kind, c.Workload.Namespace, c.Workload.Name)
		if ok && cur != c.Replicas {
			w.trace.addOnce(Event{Kind: "cmd", Obj: c.Workload.String(), Msg: fmt.Sprintf("scale %d -> %d%s", cur, c.Replicas, dry),
				Reason: string(c.Action)}, w.cpCfg.DryRun)
			if w.cpCfg.DryRun {
				w.dryRunAttempts++
			}
		}
	case command.DeletePod:
		var p corev1.Pod
		if err := w.store.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &p); err == nil && p.UID == c.UID {
			w.trace.addOnce(Event{Kind: "cmd", Obj: "Pod/" + c.Namespace + "/" + c.Name, Msg: "delete" + dry, Reason: string(c.Action)}, w.cpCfg.DryRun)
			if w.cpCfg.DryRun {
				w.dryRunAttempts++
			}
		}
	case command.CreateHostTask:
		var t v1alpha1.SmbHostTask
		if err := w.store.Get(ctx, client.ObjectKey{Name: c.Name}, &t); apierrors.IsNotFound(err) {
			w.trace.add(Event{Kind: "cmd", Obj: "SmbHostTask/" + c.Name, Msg: "create " + actionsSummary(w, c.Actions)})
		}
	case command.DeleteHostTask:
		w.trace.add(Event{Kind: "cmd", Obj: "SmbHostTask/" + c.Name, Msg: "delete"})
	case command.EmitEvent, command.PatchSmbMountRemediationMirror:
	}
	return r.inner.ExecuteCluster(ctx, cmd)
}

func actionsSummary(w *World, acts []v1alpha1.HostAction) string {
	parts := make([]string, 0, len(acts))
	for _, a := range acts {
		s := string(a.Type) + ":" + w.trace.abbrev(a.Path)
		if a.OnlyIf != "" {
			s += "?" + string(a.OnlyIf)
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// recorder is an events.EventRecorder that traces Events.
type recorder struct {
	w      *World
	source string
}

func (r *recorder) Eventf(regarding, _ runtime.Object, eventtype, reason, _, note string, args ...any) {
	msg := note
	if len(args) > 0 {
		msg = fmt.Sprintf(note, args...)
	}
	obj := ""
	if regarding != nil {
		if acc, err := meta.Accessor(regarding); err == nil {
			kind := regarding.GetObjectKind().GroupVersionKind().Kind
			if kind == "" {
				kind = strings.TrimPrefix(fmt.Sprintf("%T", regarding), "*v1alpha1.")
			}
			obj = kind + "/" + acc.GetName()
		}
	}
	r.w.checkSecret("event", msg)
	r.w.trace.add(Event{Kind: "event", Obj: obj, Reason: eventtype + "/" + reason, Msg: r.w.trace.abbrevAll(msg), Src: r.source})
}

// --- crash points ---------------------------------------------------------

// simCrash is the panic value of an injected process crash.
type simCrash struct{ where string }

// protect runs fn and reports whether it crashed with an injected simCrash.
func protect(fn func()) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(simCrash); ok {
				crashed = true
				return
			}
			panic(r)
		}
	}()
	fn()
	return false
}

type cpCrashPoint struct {
	stage    v1alpha1.Stage
	downtime time.Duration
	fired    bool
}

// CrashControlPlaneAfterWrite arms a crash of the control plane right after
// it persisted (write-ahead) a remediation status entering stage, before
// executing that Step's commands.
func (w *World) CrashControlPlaneAfterWrite(stage v1alpha1.Stage, downtime time.Duration) {
	w.cpCrash = &cpCrashPoint{stage: stage, downtime: downtime}
}

func (w *World) cpCrashDowntime() time.Duration {
	if w.cpCrash != nil {
		return w.cpCrash.downtime
	}
	return 0
}

func (w *World) icCPStatusUpdate(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if err := c.SubResource(sub).Update(ctx, obj, opts...); err != nil {
		return err
	}
	r, ok := obj.(*v1alpha1.SmbMountRemediation)
	if !ok {
		return nil
	}
	w.obs.observeRemediation(r)
	if cp := w.cpCrash; cp != nil && !cp.fired {
		if r.Status.Stage == cp.stage {
			cp.fired = true
			w.trace.add(Event{Kind: "crash", Obj: "SmbMountRemediation/" + r.Name,
				Msg: "control plane dies right after persisting stage " + string(cp.stage) + " (before its commands)"})
			panic(simCrash{where: "controlplane"})
		}
	}
	return nil
}

// icCPCreate crashes the control plane right after it created a
// remediation, before its first Step (a crash "at pending").
func (w *World) icCPCreate(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
	if err := c.Create(ctx, obj, opts...); err != nil {
		return err
	}
	if cp := w.cpCrash; cp != nil && !cp.fired && cp.stage == v1alpha1.StagePending {
		if r, ok := obj.(*v1alpha1.SmbMountRemediation); ok {
			cp.fired = true
			w.trace.add(Event{Kind: "crash", Obj: "SmbMountRemediation/" + r.Name,
				Msg: "control plane dies right after creating the remediation (before its first Step)"})
			panic(simCrash{where: "controlplane"})
		}
	}
	return nil
}

type workerCrashPoint struct {
	node      string
	taskMatch string // substring of the task name
	after     int    // crash after this many persisted results of the task
	seen      map[string]int
	fired     bool
}

// CrashWorkerMidTask arms a crash of node's worker once a task whose name
// contains taskMatch has persisted `after` action results: the next action
// runs on the host but its result is never written.
func (w *World) CrashWorkerMidTask(node, taskMatch string, after int) {
	w.workerCrash = &workerCrashPoint{node: node, taskMatch: taskMatch, after: after, seen: map[string]int{}}
}

func (n *Node) icWorkerStatusUpdate(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	wc := n.w.workerCrash
	if t, ok := obj.(*v1alpha1.SmbHostTask); ok && wc != nil && !wc.fired && wc.node == n.Name &&
		strings.Contains(t.Name, wc.taskMatch) && len(t.Status.Results) > wc.after {
		wc.fired = true
		n.w.trace.add(Event{Kind: "crash", Obj: "SmbHostTask/" + t.Name,
			Msg: "worker dies after running action " + strconv.Itoa(len(t.Status.Results)) + " before persisting its result"})
		panic(simCrash{where: "worker"})
	}
	return c.SubResource(sub).Update(ctx, obj, opts...)
}

func reconcileRequest(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}
