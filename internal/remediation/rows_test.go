package remediation

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

type rowCase struct {
	name     string
	typ      v1alpha1.RemediationType
	from, to v1alpha1.Stage
	reason   string
	// run drives the world and returns the results to search for the transition.
	run   func(t *testing.T) []Result
	check func(t *testing.T, res []Result)
}

// at builds a world (after prep), runs a fresh remediation until it waits in stage.
func at(t *testing.T, typ v1alpha1.RemediationType, stage v1alpha1.Stage, prep func(w *world)) (*world, *v1alpha1.SmbMountRemediation, Config) {
	t.Helper()
	w := newWorld()
	obj := workloadRem()
	if typ == v1alpha1.RemediationTypeSweep {
		addSweepMount(w)
		obj = newRemediation(v1alpha1.RemediationTypeSweep, v1alpha1.SmbMountName(testNode, "pv-s"))
	}
	if prep != nil {
		prep(w)
	}
	cfg := testCfg()
	w.runUntil(t, obj, cfg, stage)
	return w, obj, cfg
}

// addSweepMount adds pv-s: dead, unreferenced globalmount plus a non-empty orphan pod dir.
func addSweepMount(w *world) {
	sm := w.addMount("pv-s", v1alpha1.HealthBroken, &v1alpha1.GlobalmountStatus{Mounted: true, FSType: "cifs", DirExists: true})
	sm.Status.OrphanPodDirs = []v1alpha1.OrphanPodDir{{UID: "u-gone", Path: podPath("u-gone", "pv-s")}}
}

// runToEnd runs a fresh remediation to done and returns every result.
func runToEnd(t *testing.T, typ v1alpha1.RemediationType, prep func(w *world)) []Result {
	t.Helper()
	w := newWorld()
	obj := workloadRem()
	if typ == v1alpha1.RemediationTypeSweep {
		addSweepMount(w)
		obj = newRemediation(v1alpha1.RemediationTypeSweep, v1alpha1.SmbMountName(testNode, "pv-s"))
	}
	if prep != nil {
		prep(w)
	}
	cfg := testCfg()
	var out []Result
	for i := 0; i < 400 && !obj.Status.Stage.Terminal(); i++ {
		out = append(out, w.step(t, obj, cfg))
		w.now = w.now.Add(3 * time.Second)
	}
	if !obj.Status.Stage.Terminal() {
		t.Fatalf("did not terminate: %s", trace(obj))
	}
	return out
}

// once advances the clock by d, optionally mutates, then steps n times.
func once(t *testing.T, w *world, obj *v1alpha1.SmbMountRemediation, cfg Config, d time.Duration, n int) []Result {
	t.Helper()
	w.now = w.now.Add(d)
	var out []Result
	for i := 0; i < n; i++ {
		out = append(out, w.step(t, obj, cfg))
	}
	return out
}

func dry(obj *v1alpha1.SmbMountRemediation) { obj.Spec.DryRun = true }

func noScaleOrDelete(t *testing.T, res []Result) {
	t.Helper()
	for _, r := range res {
		if n := len(cmdsOf[command.ScaleWorkload](r.Commands)) + len(cmdsOf[command.DeletePod](r.Commands)); n > 0 {
			t.Fatalf("dry run emitted %d scale/delete commands: %v", n, keys(r.Commands))
		}
		for _, c := range cmdsOf[command.CreateHostTask](r.Commands) {
			if !c.DryRun {
				t.Fatalf("dry run created live host task %s", c.Name)
			}
		}
	}
}

func outcome(want v1alpha1.Outcome) func(t *testing.T, res []Result) {
	return func(t *testing.T, res []Result) {
		t.Helper()
		if got := res[len(res)-1].Status.Outcome; got != want {
			t.Fatalf("outcome %q, want %q", got, want)
		}
	}
}

func hasEvent(reason string) func(t *testing.T, res []Result) {
	return func(t *testing.T, res []Result) {
		t.Helper()
		for _, r := range res {
			for _, e := range cmdsOf[command.EmitEvent](r.Commands) {
				if e.Reason == reason {
					return
				}
			}
		}
		t.Fatalf("no %s event", reason)
	}
}

const (
	W = v1alpha1.RemediationTypeWorkload
	S = v1alpha1.RemediationTypeSweep
)

func rowCases() []rowCase {
	var cs []rowCase
	add := func(c rowCase) { cs = append(cs, c) }
	stepAt := func(typ v1alpha1.RemediationType, stage v1alpha1.Stage, prep func(*world), mut func(*world, *v1alpha1.SmbMountRemediation), d time.Duration, n int) func(t *testing.T) []Result {
		return func(t *testing.T) []Result {
			w, obj, cfg := at(t, typ, stage, prep)
			if mut != nil {
				mut(w, obj)
			}
			return once(t, w, obj, cfg, d, n)
		}
	}
	end := func(typ v1alpha1.RemediationType, prep func(*world)) func(t *testing.T) []Result {
		return func(t *testing.T) []Result { return runToEnd(t, typ, prep) }
	}
	running := func(w *world) { w.taskPhase = v1alpha1.HostTaskRunning }
	noDrain := func(w *world) { w.autoDrain = false }
	stuck := func(w *world) { w.stuck = true }
	// A Sweep only waits for kubelet while the globalmount dir holds data.
	stuckDirty := func(w *world) { w.stuck, w.refill = true, true }
	setDry := func(_ *world, o *v1alpha1.SmbMountRemediation) { dry(o) }

	// ---- Workload ----
	add(rowCase{name: "created", typ: W, from: "", to: v1alpha1.StagePending, reason: ReasonCreated, run: end(W, nil)})
	add(rowCase{name: "no_scope_conflict", typ: W, from: v1alpha1.StagePending, to: v1alpha1.StagePlanning, reason: ReasonNoScopeConflict, run: end(W, nil)})
	add(rowCase{name: "superseded", typ: W, from: v1alpha1.StagePending, to: v1alpha1.StageFinalizing, reason: ReasonSuperseded,
		run: stepAt(W, v1alpha1.StagePending, func(w *world) {
			// Held in pending by another node's remediation of the workload.
			w.others = []v1alpha1.SmbMountRemediation{olderActive("n2", dep("app"))}
		}, func(w *world, o *v1alpha1.SmbMountRemediation) {
			w.others = nil
			for _, sm := range w.mounts {
				sm.Status.Health = v1alpha1.HealthHealthy
				for i := range sm.Status.PodMounts {
					sm.Status.PodMounts[i].Health = v1alpha1.HealthHealthy
				}
			}
			o.Status.Conditions = append(o.Status.Conditions, metav1.Condition{
				Type: v1alpha1.ConditionScopeConflict, Status: metav1.ConditionTrue, Reason: "ScopeConflict"})
		}, 0, 1),
		check: outcome(v1alpha1.OutcomeHealed)})
	add(rowCase{name: "probe_completed", typ: W, from: v1alpha1.StagePlanning, to: v1alpha1.StageDraining, reason: ReasonProbeCompleted,
		run: stepAt(W, v1alpha1.StagePlanning, nil, nil, 0, 1)})
	add(rowCase{name: "probe_timeout", typ: W, from: v1alpha1.StagePlanning, to: v1alpha1.StageDraining, reason: ReasonProbeTimeout,
		run: stepAt(W, v1alpha1.StagePlanning, running, nil, 15*time.Second, 1)})
	add(rowCase{name: "planning dry_run", typ: W, from: v1alpha1.StagePlanning, to: v1alpha1.StageDraining, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StagePlanning, running, setDry, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "no_owner", typ: W, from: v1alpha1.StagePlanning, to: v1alpha1.StageFinalizing, reason: ReasonNoOwner,
		run: end(W, func(w *world) {
			for i := range w.pods {
				w.pods[i].Owner = nil
			}
		}), check: outcome(v1alpha1.OutcomeSkippedNoOwner)})
	add(rowCase{name: "planning scope_conflict", typ: W, from: v1alpha1.StagePlanning, to: v1alpha1.StagePending, reason: ReasonScopeConflict,
		run: stepAt(W, v1alpha1.StagePlanning, nil, func(w *world, _ *v1alpha1.SmbMountRemediation) {
			w.others = []v1alpha1.SmbMountRemediation{olderActive("n2", dep("co"))}
		}, 0, 1),
		check: func(t *testing.T, res []Result) {
			if res[0].Status.Scope != nil {
				t.Fatalf("scope not cleared")
			}
		}})
	add(rowCase{name: "drained", typ: W, from: v1alpha1.StageDraining, to: v1alpha1.StageReleasingMounts, reason: ReasonDrained,
		run: stepAt(W, v1alpha1.StageDraining, nil, nil, 0, 1)})
	add(rowCase{name: "scale_down_timeout", typ: W, from: v1alpha1.StageDraining, to: v1alpha1.StageReleasingMounts, reason: ReasonScaleDownTimeout,
		run: stepAt(W, v1alpha1.StageDraining, noDrain, nil, 120*time.Second, 1), check: hasEvent("DrainTimeout")})
	add(rowCase{name: "draining dry_run", typ: W, from: v1alpha1.StageDraining, to: v1alpha1.StageReleasingMounts, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StageDraining, noDrain, setDry, 0, 1), check: noScaleOrDelete})
	orphan := func(w *world, _ *v1alpha1.SmbMountRemediation) {
		w.mounts["pv-a"].Status.OrphanPodDirs = []v1alpha1.OrphanPodDir{{UID: "u-app-1", Path: podPath("u-app-1", "pv-a"), Mounted: true}}
	}
	add(rowCase{name: "releasing host_task_completed", typ: W, from: v1alpha1.StageReleasingMounts, to: v1alpha1.StageAwaitingUnstage, reason: ReasonHostTaskCompleted,
		run: stepAt(W, v1alpha1.StageDraining, nil, orphan, 0, 2)})
	add(rowCase{name: "releasing nothing_to_do", typ: W, from: v1alpha1.StageReleasingMounts, to: v1alpha1.StageAwaitingUnstage, reason: ReasonNothingToDo,
		run: end(W, nil)})
	add(rowCase{name: "releasing dry_run", typ: W, from: v1alpha1.StageReleasingMounts, to: v1alpha1.StageAwaitingUnstage, reason: ReasonDryRun,
		run:   stepAt(W, v1alpha1.StageDraining, running, func(w *world, o *v1alpha1.SmbMountRemediation) { orphan(w, o); dry(o) }, 0, 2),
		check: noScaleOrDelete})
	add(rowCase{name: "awaiting_unstage nothing_to_do", typ: W, from: v1alpha1.StageAwaitingUnstage, to: v1alpha1.StageQuarantining, reason: ReasonNothingToDo,
		run: end(W, func(w *world) { w.mounts["pv-a"].Status.Globalmount.Mounted = false })})
	add(rowCase{name: "unstaged", typ: W, from: v1alpha1.StageAwaitingUnstage, to: v1alpha1.StageQuarantining, reason: ReasonUnstaged,
		run: stepAt(W, v1alpha1.StageAwaitingUnstage, nil, func(w *world, _ *v1alpha1.SmbMountRemediation) {
			w.mounts["pv-a"].Status.Globalmount.Mounted = false
		}, 0, 1)})
	add(rowCase{name: "force_unmounted", typ: W, from: v1alpha1.StageAwaitingUnstage, to: v1alpha1.StageQuarantining, reason: ReasonForceUnmounted,
		run: stepAt(W, v1alpha1.StageAwaitingUnstage, nil, nil, 30*time.Second, 2),
		check: func(t *testing.T, res []Result) {
			c := cmdsOf[command.CreateHostTask](res[0].Commands)
			if len(c) != 1 || c[0].Actions[0].Type != v1alpha1.HostActionForceUnmount || c[0].Actions[0].Path != gmPath("pv-a") {
				t.Fatalf("want force-unmount task for pv-a gm, got %+v", c)
			}
		}})
	add(rowCase{name: "awaiting_unstage dry_run", typ: W, from: v1alpha1.StageAwaitingUnstage, to: v1alpha1.StageQuarantining, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StageAwaitingUnstage, nil, setDry, 0, 2), check: noScaleOrDelete})
	add(rowCase{name: "quarantining host_task_completed", typ: W, from: v1alpha1.StageQuarantining, to: v1alpha1.StageAwaitingUnstageCompletion, reason: ReasonHostTaskCompleted,
		run: stepAt(W, v1alpha1.StageQuarantining, nil, nil, 0, 1)})
	add(rowCase{name: "quarantining nothing_to_do", typ: W, from: v1alpha1.StageQuarantining, to: v1alpha1.StageAwaitingUnstageCompletion, reason: ReasonNothingToDo,
		run: stepAt(W, v1alpha1.StageAwaitingUnstage, nil, func(w *world, _ *v1alpha1.SmbMountRemediation) {
			delete(w.mounts, "pv-a")
			delete(w.mounts, "pv-b")
		}, 0, 1)})
	add(rowCase{name: "quarantining dry_run", typ: W, from: v1alpha1.StageQuarantining, to: v1alpha1.StageAwaitingUnstageCompletion, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StageQuarantining, func(w *world) {
			running(w)
			w.mounts["pv-a"].Status.Globalmount.Mounted = false
		}, setDry, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "unstage_completed", typ: W, from: v1alpha1.StageAwaitingUnstageCompletion, to: v1alpha1.StageRestartingDriver, reason: ReasonUnstageCompleted,
		run: stepAt(W, v1alpha1.StageAwaitingUnstageCompletion, stuck, func(w *world, _ *v1alpha1.SmbMountRemediation) {
			w.stuck = false
			w.mounts["pv-a"].Status.Globalmount.DirExists = false
		}, 0, 1)})
	add(rowCase{name: "unstage_timeout", typ: W, from: v1alpha1.StageAwaitingUnstageCompletion, to: v1alpha1.StageRestartingDriver, reason: ReasonUnstageTimeout,
		run: stepAt(W, v1alpha1.StageAwaitingUnstageCompletion, stuck, nil, 300*time.Second, 1), check: hasEvent("UnstagePinned")})
	add(rowCase{name: "awaiting_unstage_completion dry_run", typ: W, from: v1alpha1.StageAwaitingUnstageCompletion, to: v1alpha1.StageRestartingDriver, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StageAwaitingUnstageCompletion, stuck, setDry, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "driver_ready", typ: W, from: v1alpha1.StageRestartingDriver, to: v1alpha1.StageScalingUp, reason: ReasonDriverReady,
		run: stepAt(W, v1alpha1.StageRestartingDriver, nil, nil, 0, 1)})
	add(rowCase{name: "driver_restart_timeout", typ: W, from: v1alpha1.StageRestartingDriver, to: v1alpha1.StageScalingUp, reason: ReasonDriverRestartTimeout,
		run: stepAt(W, v1alpha1.StageRestartingDriver, func(w *world) { w.autoDriver = false }, nil, 90*time.Second, 1)})
	add(rowCase{name: "no_driver_pod", typ: W, from: v1alpha1.StageRestartingDriver, to: v1alpha1.StageScalingUp, reason: ReasonNoDriverPod,
		run: end(W, func(w *world) { w.driver = nil })})
	add(rowCase{name: "restarting_driver dry_run", typ: W, from: v1alpha1.StageRestartingDriver, to: v1alpha1.StageScalingUp, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StageRestartingDriver, func(w *world) { w.autoDriver = false }, setDry, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "scaled_up", typ: W, from: v1alpha1.StageScalingUp, to: v1alpha1.StageVerifying, reason: ReasonScaledUp,
		run: stepAt(W, v1alpha1.StageScalingUp, nil, nil, 0, 1)})
	add(rowCase{name: "scaling_up dry_run", typ: W, from: v1alpha1.StageScalingUp, to: v1alpha1.StageVerifying, reason: ReasonDryRun,
		run: stepAt(W, v1alpha1.StageScalingUp, func(w *world) { w.spawn = false }, func(w *world, o *v1alpha1.SmbMountRemediation) {
			for _, wo := range w.workloads {
				wo.Replicas = 0
			}
			dry(o)
		}, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "verified", typ: W, from: v1alpha1.StageVerifying, to: v1alpha1.StageFinalizing, reason: ReasonVerified,
		run: end(W, nil), check: outcome(v1alpha1.OutcomeHealed)})
	add(rowCase{name: "verify_timeout", typ: W, from: v1alpha1.StageVerifying, to: v1alpha1.StageFinalizing, reason: ReasonVerifyTimeout,
		run:   stepAt(W, v1alpha1.StageVerifying, func(w *world) { w.newHealth = v1alpha1.HealthBroken }, nil, 90*time.Second, 1),
		check: outcome(v1alpha1.OutcomeVerifyFailed)})
	add(rowCase{name: "verifying dry_run", typ: W, from: v1alpha1.StageVerifying, to: v1alpha1.StageFinalizing, reason: ReasonDryRun,
		run:   stepAt(W, v1alpha1.StageVerifying, func(w *world) { w.newHealth = v1alpha1.HealthUnknown }, setDry, 0, 1),
		check: outcome(v1alpha1.OutcomeDryRun)})
	for _, rb := range []struct {
		reason string
		mut    func(*world, *v1alpha1.SmbMountRemediation)
		d      time.Duration
	}{
		{ReasonDeleted, func(w *world, _ *v1alpha1.SmbMountRemediation) { w.deleting = true }, 0},
		{ReasonAborted, func(w *world, _ *v1alpha1.SmbMountRemediation) { w.aborted = true }, 0},
		{ReasonEpisodeTimeout, nil, 30 * time.Minute},
		{ReasonWorkloadsDeleted, func(w *world, _ *v1alpha1.SmbMountRemediation) {
			w.workloads[dep("app").String()].Exists = false
			w.workloads[dep("co").String()].Exists = false
		}, 0},
	} {
		add(rowCase{name: "rolling_back " + rb.reason, typ: W, from: v1alpha1.StageDraining, to: v1alpha1.StageRollingBack, reason: rb.reason,
			run: stepAt(W, v1alpha1.StageDraining, noDrain, rb.mut, rb.d, 1), check: hasEvent("RollingBack")})
	}
	add(rowCase{name: "restored", typ: W, from: v1alpha1.StageRollingBack, to: v1alpha1.StageFinalizing, reason: ReasonRestored,
		run:   stepAt(W, v1alpha1.StageDraining, noDrain, func(w *world, _ *v1alpha1.SmbMountRemediation) { w.aborted = true }, 0, 2),
		check: outcome(v1alpha1.OutcomeRolledBack)})
	add(rowCase{name: "restore_timeout", typ: W, from: v1alpha1.StageRollingBack, to: v1alpha1.StageFinalizing, reason: ReasonRestoreTimeout,
		run: func(t *testing.T) []Result {
			w, obj, cfg := at(t, W, v1alpha1.StageDraining, noDrain)
			w.aborted = true
			r1 := Step(obj, w.obs(), cfg) // rollback commands are never executed
			obj.Status = r1.Status
			w.now = w.now.Add(120 * time.Second)
			return []Result{r1, w.step(t, obj, cfg)}
		}, check: hasEvent("RestoreTimeout")})
	add(rowCase{name: "rolling_back dry_run", typ: W, from: v1alpha1.StageRollingBack, to: v1alpha1.StageFinalizing, reason: ReasonDryRun,
		run:   stepAt(W, v1alpha1.StageDraining, noDrain, func(w *world, o *v1alpha1.SmbMountRemediation) { w.aborted = true; dry(o) }, 0, 1),
		check: outcome(v1alpha1.OutcomeRolledBack)})
	add(rowCase{name: "finalized", typ: W, from: v1alpha1.StageFinalizing, to: v1alpha1.StageDone, reason: ReasonFinalized, run: end(W, nil)})

	// ---- Sweep ----
	add(rowCase{name: "sweep created", typ: S, from: "", to: v1alpha1.StagePlanning, reason: ReasonCreated, run: end(S, nil)})
	add(rowCase{name: "sweep probe_completed", typ: S, from: v1alpha1.StagePlanning, to: v1alpha1.StageSweeping, reason: ReasonProbeCompleted,
		run: stepAt(S, v1alpha1.StagePlanning, nil, nil, 0, 1)})
	add(rowCase{name: "sweep probe_timeout", typ: S, from: v1alpha1.StagePlanning, to: v1alpha1.StageSweeping, reason: ReasonProbeTimeout,
		run: stepAt(S, v1alpha1.StagePlanning, running, nil, 15*time.Second, 1)})
	add(rowCase{name: "sweep planning dry_run", typ: S, from: v1alpha1.StagePlanning, to: v1alpha1.StageSweeping, reason: ReasonDryRun,
		run: stepAt(S, v1alpha1.StagePlanning, running, setDry, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "nothing_to_sweep", typ: S, from: v1alpha1.StagePlanning, to: v1alpha1.StageFinalizing, reason: ReasonNothingToSweep,
		run: stepAt(S, v1alpha1.StagePlanning, nil, func(w *world, _ *v1alpha1.SmbMountRemediation) {
			w.addPod("app", "app-2", "u-app-2", map[string]v1alpha1.Health{"pv-s": v1alpha1.HealthHealthy})
		}, 0, 1), check: outcome(v1alpha1.OutcomeHealed)})
	add(rowCase{name: "sweeping host_task_completed", typ: S, from: v1alpha1.StageSweeping, to: v1alpha1.StageAwaitingUnstageCompletion, reason: ReasonHostTaskCompleted,
		run: end(S, nil), check: outcome(v1alpha1.OutcomeHealed)})
	add(rowCase{name: "sweeping nothing_to_do", typ: S, from: v1alpha1.StageSweeping, to: v1alpha1.StageAwaitingUnstageCompletion, reason: ReasonNothingToDo,
		run: stepAt(S, v1alpha1.StagePlanning, running, func(_ *world, o *v1alpha1.SmbMountRemediation) {
			o.Status.Stage = v1alpha1.StageSweeping
			o.Status.Scope = &v1alpha1.RemediationScope{PVs: []string{"pv-gone"}}
		}, 0, 1)})
	add(rowCase{name: "sweeping dry_run", typ: S, from: v1alpha1.StageSweeping, to: v1alpha1.StageAwaitingUnstageCompletion, reason: ReasonDryRun,
		run: stepAt(S, v1alpha1.StageSweeping, running, setDry, 0, 1), check: noScaleOrDelete})
	add(rowCase{name: "host_task_failed", typ: S, from: v1alpha1.StageSweeping, to: v1alpha1.StageFinalizing, reason: ReasonHostTaskFailed,
		run: end(S, func(w *world) { w.taskPhase = v1alpha1.HostTaskFailed }), check: outcome(v1alpha1.OutcomeFailed)})
	add(rowCase{name: "sweep unstage_completed", typ: S, from: v1alpha1.StageAwaitingUnstageCompletion, to: v1alpha1.StageFinalizing, reason: ReasonUnstageCompleted,
		run: end(S, nil)})
	add(rowCase{name: "sweep unstage_timeout", typ: S, from: v1alpha1.StageAwaitingUnstageCompletion, to: v1alpha1.StageFinalizing, reason: ReasonUnstageTimeout,
		run: stepAt(S, v1alpha1.StageAwaitingUnstageCompletion, stuckDirty, nil, 300*time.Second, 1), check: hasEvent("UnstagePinned")})
	add(rowCase{name: "sweep awaiting_unstage_completion dry_run", typ: S, from: v1alpha1.StageAwaitingUnstageCompletion, to: v1alpha1.StageFinalizing, reason: ReasonDryRun,
		run: stepAt(S, v1alpha1.StageAwaitingUnstageCompletion, stuckDirty, setDry, 0, 1), check: outcome(v1alpha1.OutcomeDryRun)})
	for _, rb := range []struct {
		reason string
		mut    func(*world, *v1alpha1.SmbMountRemediation)
		d      time.Duration
	}{
		{ReasonDeleted, func(w *world, _ *v1alpha1.SmbMountRemediation) { w.deleting = true }, 0},
		{ReasonAborted, func(w *world, _ *v1alpha1.SmbMountRemediation) { w.aborted = true }, 0},
		{ReasonEpisodeTimeout, nil, 30 * time.Minute},
	} {
		add(rowCase{name: "sweep rolling_back " + rb.reason, typ: S, from: v1alpha1.StageSweeping, to: v1alpha1.StageRollingBack, reason: rb.reason,
			run: stepAt(S, v1alpha1.StageSweeping, running, rb.mut, rb.d, 1)})
	}
	add(rowCase{name: "sweep restored", typ: S, from: v1alpha1.StageRollingBack, to: v1alpha1.StageFinalizing, reason: ReasonRestored,
		run:   stepAt(S, v1alpha1.StageSweeping, running, func(w *world, _ *v1alpha1.SmbMountRemediation) { w.aborted = true }, 0, 1),
		check: outcome(v1alpha1.OutcomeRolledBack)})
	add(rowCase{name: "sweep rolling_back dry_run", typ: S, from: v1alpha1.StageRollingBack, to: v1alpha1.StageFinalizing, reason: ReasonDryRun,
		run:   stepAt(S, v1alpha1.StageSweeping, running, func(w *world, o *v1alpha1.SmbMountRemediation) { w.aborted = true; dry(o) }, 0, 1),
		check: outcome(v1alpha1.OutcomeRolledBack)})
	add(rowCase{name: "sweep finalized", typ: S, from: v1alpha1.StageFinalizing, to: v1alpha1.StageDone, reason: ReasonFinalized, run: end(S, nil)})
	return cs
}

func olderActive(node string, refs ...v1alpha1.WorkloadRef) v1alpha1.SmbMountRemediation {
	r := v1alpha1.SmbMountRemediation{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-older", CreationTimestamp: metav1.NewTime(t0.Add(-time.Minute))},
		Spec:       v1alpha1.SmbMountRemediationSpec{NodeName: node, Type: v1alpha1.RemediationTypeWorkload},
		Status:     v1alpha1.SmbMountRemediationStatus{Stage: v1alpha1.StageDraining, Scope: &v1alpha1.RemediationScope{}},
	}
	for _, ref := range refs {
		r.Status.Scope.Workloads = append(r.Status.Scope.Workloads, v1alpha1.ScopedWorkload{WorkloadRef: ref, OriginalReplicas: 1})
	}
	return r
}

// TestTransitionRows runs one case per (rule, reason) of the transition
// table and fails if any table entry has no case.
func TestTransitionRows(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range rowCases() {
		idx := RuleIndex(c.typ, c.from, c.to, c.reason)
		if idx < 0 {
			t.Fatalf("case %q: %s -> %s (%s) is not a %s rule", c.name, c.from, c.to, c.reason, c.typ)
		}
		covered[fmt.Sprintf("%d/%s", idx, c.reason)] = true
		t.Run(string(c.typ)+"/"+c.name, func(t *testing.T) {
			res := c.run(t)
			found := false
			for _, r := range res {
				if hasTransition(r, c.from, c.to, c.reason) {
					found = true
				}
			}
			if !found {
				var got []Transition
				for _, r := range res {
					got = append(got, r.Transitions...)
				}
				t.Fatalf("transition %s -> %s (%s) not produced; got %+v", c.from, c.to, c.reason, got)
			}
			if c.check != nil {
				c.check(t, res)
			}
		})
	}
	for i, r := range Rules() {
		for _, reason := range r.Reasons {
			if !covered[fmt.Sprintf("%d/%s", i, reason)] {
				t.Errorf("rule %d (%s %v -> %s) reason %s has no test case", i, r.Machine, r.From, r.To, reason)
			}
		}
	}
}
