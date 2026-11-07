package remediation

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

type snapshot struct {
	obj *v1alpha1.SmbMountRemediation
	obs Observation
}

// snapshots records (obj, obs) before every Step of several scenarios.
func snapshots(t testing.TB) []snapshot {
	var out []snapshot
	type scen struct {
		typ  v1alpha1.RemediationType
		prep func(*world)
	}
	for _, sc := range []scen{
		{W, nil},
		{W, func(w *world) { w.newHealth = v1alpha1.HealthBroken; w.autoDrain = false; w.stuck = true }},
		{W, func(w *world) { w.aborted = false; w.autoDriver = false; w.spawn = false }},
		{S, nil},
		{S, func(w *world) { w.taskPhase = v1alpha1.HostTaskFailed }},
	} {
		w := newWorld()
		obj := workloadRem()
		if sc.typ == S {
			addSweepMount(w)
			obj = newRemediation(S, v1alpha1.SmbMountName(testNode, "pv-s"))
		}
		if sc.prep != nil {
			sc.prep(w)
		}
		w.others = []v1alpha1.SmbMountRemediation{olderActive("n2", dep("unrelated"))}
		cfg := testCfg()
		for i := 0; i < 400 && !obj.Status.Stage.Terminal(); i++ {
			out = append(out, snapshot{obj: obj.DeepCopy(), obs: w.obs()})
			res := Step(obj, w.obs(), cfg)
			obj.Status = res.Status
			w.apply(res.Commands)
			w.now = w.now.Add(3 * time.Second)
		}
		out = append(out, snapshot{obj: obj.DeepCopy(), obs: w.obs()})
	}
	if len(out) < 20 {
		t.Fatalf("too few snapshots: %d", len(out))
	}
	return out
}

// TestConvergence: Step is a fixed point on its own output for the same
// observation: same status, no transitions, and the wait-state commands are
// re-emitted identically (a subset of the first step's commands; the
// difference is transition-only Events / non-waited ProbeNow tasks).
func TestConvergence(t *testing.T) {
	cfg := testCfg()
	for i, s := range snapshots(t) {
		r1 := Step(s.obj, s.obs, cfg)
		o2 := s.obj.DeepCopy()
		o2.Status = r1.Status
		r2 := Step(o2, s.obs, cfg)
		if len(r2.Transitions) != 0 {
			t.Fatalf("snapshot %d (%s): second step transitioned %+v", i, s.obj.Status.Stage, r2.Transitions)
		}
		if !reflect.DeepEqual(r1.Status, r2.Status) {
			t.Fatalf("snapshot %d (%s): status changed on re-step", i, r1.Status.Stage)
		}
		k1 := keys(r1.Commands)
		for _, k := range keys(r2.Commands) {
			if !slices.Contains(k1, k) {
				t.Fatalf("snapshot %d (%s): re-step emitted new command %s (first: %v)", i, r1.Status.Stage, k, k1)
			}
		}
		if len(r1.Transitions) == 0 && !slices.Equal(k1, keys(r2.Commands)) {
			t.Fatalf("snapshot %d (%s): waiting step not idempotent: %v vs %v", i, r1.Status.Stage, k1, keys(r2.Commands))
		}
		r3 := Step(o2, s.obs, cfg)
		if !reflect.DeepEqual(r2, r3) {
			t.Fatalf("snapshot %d: Step is not a pure function", i)
		}
		if !r1.Status.Stage.Terminal() && (r1.RequeueAfter <= 0 || r1.RequeueAfter > cfg.PollInterval) {
			t.Fatalf("snapshot %d (%s): requeue %v", i, r1.Status.Stage, r1.RequeueAfter)
		}
	}
}

// TestGuardFlipsAfterApplying: applying a waiting step's commands to the
// world flips the guard.
func TestGuardFlipsAfterApplying(t *testing.T) {
	for _, stage := range []v1alpha1.Stage{v1alpha1.StageDraining, v1alpha1.StageRestartingDriver, v1alpha1.StageScalingUp} {
		w, obj, cfg := at(t, W, stage, func(w *world) {
			w.spawn = stage != v1alpha1.StageScalingUp
			w.autoDrain = stage != v1alpha1.StageDraining
			w.autoDriver = stage != v1alpha1.StageRestartingDriver
		})
		w.spawn, w.autoDrain, w.autoDriver = true, true, true
		if stage == v1alpha1.StageScalingUp {
			// The scale-up has not been executed (or is not visible) yet.
			w.workloads[dep("app").String()].Replicas = 0
			w.workloads[dep("co").String()].Replicas = 0
		}
		if stage == v1alpha1.StageRestartingDriver {
			// The delete has not been executed (or is not visible) yet.
			w.driver = []DriverPodObservation{{Namespace: "kube-system", Name: "csi-smb-node-a", UID: "drv-1", Running: true, Ready: true}}
		}
		if stage == v1alpha1.StageDraining {
			w.workloads[dep("app").String()].Replicas = 2 // Flux scaled it back up
			w.removePod("u-co-1")                         // co already drained
		}
		r1 := Step(obj, w.obs(), cfg)
		r2 := Step(obj, w.obs(), cfg) // not applied: same commands, still waiting
		if !reflect.DeepEqual(keys(r1.Commands), keys(r2.Commands)) || len(r1.Transitions) != 0 {
			t.Fatalf("%s: not idempotent while waiting", stage)
		}
		if stage == v1alpha1.StageScalingUp {
			if len(cmdsOf[command.ScaleWorkload](r1.Commands)) == 0 {
				t.Fatalf("scaling_up emitted no scale")
			}
		}
		obj.Status = r1.Status
		w.apply(r1.Commands)
		r3 := w.step(t, obj, cfg)
		if len(r3.Transitions) == 0 || r3.Transitions[0].From != stage {
			t.Fatalf("%s: guard did not flip after applying %v", stage, keys(r1.Commands))
		}
	}
}

func shuffleObs(r *rand.Rand, o Observation) Observation {
	sh := func(n int, swap func(i, j int)) { r.Shuffle(n, swap) }
	o.SmbMounts = append([]v1alpha1.SmbMount(nil), o.SmbMounts...)
	for i := range o.SmbMounts {
		sm := o.SmbMounts[i].DeepCopy()
		st := &sm.Status
		sh(len(st.PodMounts), func(a, b int) { st.PodMounts[a], st.PodMounts[b] = st.PodMounts[b], st.PodMounts[a] })
		sh(len(st.OrphanPodDirs), func(a, b int) {
			st.OrphanPodDirs[a], st.OrphanPodDirs[b] = st.OrphanPodDirs[b], st.OrphanPodDirs[a]
		})
		sh(len(st.Conditions), func(a, b int) { st.Conditions[a], st.Conditions[b] = st.Conditions[b], st.Conditions[a] })
		o.SmbMounts[i] = *sm
	}
	sh(len(o.SmbMounts), func(a, b int) { o.SmbMounts[a], o.SmbMounts[b] = o.SmbMounts[b], o.SmbMounts[a] })
	o.Pods = append([]PodObservation(nil), o.Pods...)
	for i := range o.Pods {
		pvs := append([]string(nil), o.Pods[i].PVs...)
		sh(len(pvs), func(a, b int) { pvs[a], pvs[b] = pvs[b], pvs[a] })
		o.Pods[i].PVs = pvs
	}
	sh(len(o.Pods), func(a, b int) { o.Pods[a], o.Pods[b] = o.Pods[b], o.Pods[a] })
	o.Workloads = append([]WorkloadObservation(nil), o.Workloads...)
	sh(len(o.Workloads), func(a, b int) { o.Workloads[a], o.Workloads[b] = o.Workloads[b], o.Workloads[a] })
	o.DriverPods = append([]DriverPodObservation(nil), o.DriverPods...)
	sh(len(o.DriverPods), func(a, b int) { o.DriverPods[a], o.DriverPods[b] = o.DriverPods[b], o.DriverPods[a] })
	o.HostTasks = append([]v1alpha1.SmbHostTask(nil), o.HostTasks...)
	sh(len(o.HostTasks), func(a, b int) { o.HostTasks[a], o.HostTasks[b] = o.HostTasks[b], o.HostTasks[a] })
	o.OtherActive = append([]v1alpha1.SmbMountRemediation(nil), o.OtherActive...)
	sh(len(o.OtherActive), func(a, b int) { o.OtherActive[a], o.OtherActive[b] = o.OtherActive[b], o.OtherActive[a] })
	return o
}

// FuzzDeterminism: shuffling every slice of the observation (and the
// trigger list) yields an identical Result.
func FuzzDeterminism(f *testing.F) {
	snaps := snapshots(f)
	// Extra driver pods so driver selection is order-sensitive.
	for i := range snaps {
		snaps[i].obs.DriverPods = append(snaps[i].obs.DriverPods,
			DriverPodObservation{Namespace: "kube-system", Name: "csi-smb-node-z", UID: "drv-z", Running: true, Ready: false})
	}
	for i := 0; i < len(snaps); i += 3 {
		f.Add(int64(i), uint16(i))
	}
	cfg := testCfg()
	f.Fuzz(func(t *testing.T, seed int64, pick uint16) {
		s := snaps[int(pick)%len(snaps)]
		r := rand.New(rand.NewSource(seed))
		want := Step(s.obj, s.obs, cfg)
		obj := s.obj.DeepCopy()
		r.Shuffle(len(obj.Spec.Trigger.SmbMounts), func(a, b int) {
			obj.Spec.Trigger.SmbMounts[a], obj.Spec.Trigger.SmbMounts[b] = obj.Spec.Trigger.SmbMounts[b], obj.Spec.Trigger.SmbMounts[a]
		})
		got := Step(obj, shuffleObs(r, s.obs), cfg)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("non-deterministic Step at stage %s:\nwant %v\ngot  %v", s.obj.Status.Stage, keys(want.Commands), keys(got.Commands))
		}
		tin := TriggerInput{NodeName: testNode, Now: s.obs.Now, SmbMounts: s.obs.SmbMounts, Pods: s.obs.Pods,
			Remediations: append([]v1alpha1.SmbMountRemediation{*s.obj}, s.obs.OtherActive...)}
		sobs := shuffleObs(r, s.obs)
		tin2 := TriggerInput{NodeName: testNode, Now: s.obs.Now, SmbMounts: sobs.SmbMounts, Pods: sobs.Pods,
			Remediations: append(append([]v1alpha1.SmbMountRemediation(nil), sobs.OtherActive...), *obj)}
		if !reflect.DeepEqual(Trigger(tin, cfg), Trigger(tin2, cfg)) {
			t.Fatalf("non-deterministic Trigger")
		}
	})
}

// TestResume: a status persisted mid-stage, resumed much later by a new
// leader, continues correctly and fires expired deadlines immediately.
func TestResume(t *testing.T) {
	t.Run("expired draining deadline", func(t *testing.T) {
		w, obj, cfg := at(t, W, v1alpha1.StageDraining, func(w *world) { w.autoDrain = false })
		persisted := obj.DeepCopy() // what the API server holds
		w.now = persisted.Status.Deadline.Add(5 * time.Second)
		res := w.step(t, persisted, cfg)
		if !hasTransition(res, v1alpha1.StageDraining, v1alpha1.StageReleasingMounts, ReasonScaleDownTimeout) {
			t.Fatalf("got %+v", res.Transitions)
		}
	})
	t.Run("driver deleted before crash", func(t *testing.T) {
		w, obj, cfg := at(t, W, v1alpha1.StageRestartingDriver, func(w *world) { w.autoDriver = false })
		if obj.Status.Driver == nil || obj.Status.Driver.OldPodUID != "drv-1" {
			t.Fatalf("old uid not recorded: %+v", obj.Status.Driver)
		}
		// The delete was executed, then the control plane died; the DaemonSet
		// recreated the pod meanwhile.
		w.driver = []DriverPodObservation{{Namespace: "kube-system", Name: "csi-smb-node-c", UID: "drv-3", Running: true, Ready: true}}
		w.now = w.now.Add(10 * time.Second)
		res := w.step(t, obj, cfg)
		if len(cmdsOf[command.DeletePod](res.Commands)) != 0 {
			t.Fatalf("deleted the new driver pod")
		}
		if !hasTransition(res, v1alpha1.StageRestartingDriver, v1alpha1.StageScalingUp, ReasonDriverReady) ||
			res.Status.Driver.NewPodUID != "drv-3" {
			t.Fatalf("got %+v %+v", res.Transitions, res.Status.Driver)
		}
	})
	t.Run("originalReplicas from status, not live spec", func(t *testing.T) {
		w, obj, cfg := at(t, W, v1alpha1.StageAwaitingUnstageCompletion, func(w *world) { w.stuck = true })
		// Someone fiddled with replicas while scaled down; status stays the source of truth.
		w.workloads[dep("app").String()].Replicas = 7
		w.stuck = false
		w.mounts["pv-a"].Status.Globalmount.DirExists = false
		w.runUntil(t, obj, cfg, v1alpha1.StageDone)
		if w.workloads[dep("app").String()].Replicas != 2 {
			t.Fatalf("app replicas %d, want 2", w.workloads[dep("app").String()].Replicas)
		}
	})
	t.Run("episode expired during downtime", func(t *testing.T) {
		w, obj, cfg := at(t, W, v1alpha1.StageAwaitingUnstageCompletion, func(w *world) { w.stuck = true })
		w.now = obj.Status.StartedTime.Add(cfg.EpisodeMaxDuration + time.Hour)
		res := w.step(t, obj, cfg)
		if !hasTransition(res, v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageRollingBack, ReasonEpisodeTimeout) {
			t.Fatalf("got %+v", res.Transitions)
		}
		w.runUntil(t, obj, cfg, v1alpha1.StageDone)
		if obj.Status.Outcome != v1alpha1.OutcomeRolledBack || w.workloads[dep("app").String()].Replicas != 2 ||
			w.workloads[dep("co").String()].Replicas != 1 {
			t.Fatalf("outcome %s, replicas not restored", obj.Status.Outcome)
		}
	})
}

func TestDryRunFullPath(t *testing.T) {
	w := newWorld()
	obj := workloadRem()
	obj.Spec.DryRun = true
	cfg := testCfg()
	var all []Result
	for i := 0; i < 50 && !obj.Status.Stage.Terminal(); i++ {
		all = append(all, w.step(t, obj, cfg))
		w.now = w.now.Add(3 * time.Second)
	}
	noScaleOrDelete(t, all)
	if obj.Status.Outcome != v1alpha1.OutcomeDryRun {
		t.Fatalf("outcome %s: %s", obj.Status.Outcome, trace(obj))
	}
	for _, s := range []v1alpha1.Stage{v1alpha1.StageDraining, v1alpha1.StageQuarantining, v1alpha1.StageRestartingDriver, v1alpha1.StageVerifying} {
		seen := false
		for _, tr := range obj.Status.Transitions {
			seen = seen || tr.From == s
		}
		if !seen {
			t.Fatalf("dry run skipped stage %s: %s", s, trace(obj))
		}
	}
	if len(w.created) == 0 {
		t.Fatalf("no dry-run host tasks")
	}
	for _, c := range w.created {
		if !c.DryRun {
			t.Fatalf("live task %s", c.Name)
		}
	}
	if w.workloads[dep("app").String()].Replicas != 2 || w.driver[0].UID != "drv-1" {
		t.Fatalf("dry run changed the world")
	}
	if len(all) > 6 {
		t.Fatalf("dry run waited: %d steps", len(all))
	}
}

func TestRollbackOnDeletionRestoresReplicas(t *testing.T) {
	w, obj, cfg := at(t, W, v1alpha1.StageDraining, func(w *world) { w.autoDrain = false })
	if w.workloads[dep("app").String()].Replicas != 0 {
		t.Fatalf("not scaled down")
	}
	w.deleting = true
	var actions []command.WorkloadAction
	for i := 0; i < 5 && !obj.Status.Stage.Terminal(); i++ {
		res := w.step(t, obj, cfg)
		for _, s := range cmdsOf[command.ScaleWorkload](res.Commands) {
			actions = append(actions, s.Action)
		}
	}
	if obj.Status.Outcome != v1alpha1.OutcomeRolledBack {
		t.Fatalf("outcome %s", obj.Status.Outcome)
	}
	if !slices.Contains(actions, command.ActionRollback) {
		t.Fatalf("actions %v", actions)
	}
	if w.workloads[dep("app").String()].Replicas != 2 || w.workloads[dep("co").String()].Replicas != 1 {
		t.Fatalf("replicas not restored")
	}
	if w.workloads[dep("other").String()].Replicas != 1 {
		t.Fatalf("out-of-scope workload touched")
	}
}

func TestAbortSetsCondition(t *testing.T) {
	w, obj, cfg := at(t, W, v1alpha1.StageDraining, nil)
	w.aborted = true
	res := w.step(t, obj, cfg)
	c := findCondition(res.Status.Conditions, v1alpha1.ConditionAborted)
	if c == nil || c.Status != "True" {
		t.Fatalf("no Aborted condition: %+v", res.Status.Conditions)
	}
}

func TestPendingWaitsOnScopeConflict(t *testing.T) {
	w := newWorld()
	w.others = []v1alpha1.SmbMountRemediation{olderActive("n2", dep("app"))}
	obj := workloadRem()
	cfg := testCfg()
	res := w.step(t, obj, cfg)
	if res.Status.Stage != v1alpha1.StagePending || res.RequeueAfter != cfg.PollInterval {
		t.Fatalf("stage %s requeue %v", res.Status.Stage, res.RequeueAfter)
	}
	if c := findCondition(res.Status.Conditions, v1alpha1.ConditionScopeConflict); c == nil || c.Status != "True" {
		t.Fatalf("no ScopeConflict condition")
	}
	for _, c := range res.Commands {
		if _, ok := c.(command.PatchSmbMountRemediationMirror); !ok {
			t.Fatalf("pending emitted %T", c)
		}
	}
	w.others = nil
	res = w.step(t, obj, cfg)
	if !hasTransition(res, v1alpha1.StagePending, v1alpha1.StagePlanning, ReasonNoScopeConflict) {
		t.Fatalf("got %+v", res.Transitions)
	}
	if c := findCondition(res.Status.Conditions, v1alpha1.ConditionScopeConflict); c == nil || c.Status != "False" {
		t.Fatalf("ScopeConflict not resolved")
	}
}

func TestScaleReassertAndScopeExtension(t *testing.T) {
	w, obj, cfg := at(t, W, v1alpha1.StageDraining, func(w *world) { w.autoDrain = false })
	w.step(t, obj, cfg) // observes replicas 0 -> scaled_down
	// Flux scales app back up; a new co-tenant workload lands on the dead pv-a.
	w.workloads[dep("app").String()].Replicas = 2
	w.addWorkload("late", 1, "pv-a")
	w.addPod("late", "late-1", "u-late-1", map[string]v1alpha1.Health{"pv-a": v1alpha1.HealthHealthy})
	res := Step(obj, w.obs(), cfg)
	var got []string
	for _, s := range cmdsOf[command.ScaleWorkload](res.Commands) {
		got = append(got, s.Workload.Name+":"+string(s.Action))
	}
	slices.Sort(got)
	want := []string{"app:scale_reassert", "late:scale_down"}
	if !slices.Equal(got, want) {
		t.Fatalf("scale commands %v, want %v", got, want)
	}
	found := false
	for _, sw := range res.Status.Scope.Workloads {
		if sw.Name == "late" && sw.OriginalReplicas == 1 {
			found = true
		}
	}
	if !found || !slices.ContainsFunc(res.Status.Scope.DrainPods, func(p v1alpha1.PodRef) bool { return p.UID == "u-late-1" }) {
		t.Fatalf("late not added to scope: %+v", res.Status.Scope)
	}
}

func TestVerifyElsewhere(t *testing.T) {
	w, obj, cfg := at(t, W, v1alpha1.StageVerifying, func(w *world) { w.spawn = false })
	res := once(t, w, obj, cfg, cfg.VerifyNoPodGrace, 1)
	if !hasTransition(res[0], v1alpha1.StageVerifying, v1alpha1.StageFinalizing, ReasonVerified) {
		t.Fatalf("got %+v", res[0].Transitions)
	}
	for _, v := range obj.Status.Verification {
		if v.Result != v1alpha1.VerificationElsewhere {
			t.Fatalf("verification %+v", obj.Status.Verification)
		}
	}
	if obj.Status.Outcome != v1alpha1.OutcomeHealed {
		t.Fatalf("outcome %s", obj.Status.Outcome)
	}
}

func TestTransitionsBounded(t *testing.T) {
	obj := workloadRem()
	for i := 0; i < 60; i++ {
		obj.Status.Transitions = append(obj.Status.Transitions, v1alpha1.StageTransition{To: v1alpha1.StagePending})
	}
	obj.Status.Stage = v1alpha1.StagePending
	w := newWorld()
	res := Step(obj, w.obs(), testCfg())
	if len(res.Status.Transitions) != v1alpha1.MaxTransitions {
		t.Fatalf("transitions %d", len(res.Status.Transitions))
	}
	last := res.Status.Transitions[len(res.Status.Transitions)-1]
	if last.From != v1alpha1.StagePending || last.To != v1alpha1.StagePlanning {
		t.Fatalf("newest transition dropped: %+v", last)
	}
}

func TestRequeueUsesDeadline(t *testing.T) {
	cfg := testCfg()
	cfg.PollInterval = time.Minute
	w, obj, _ := at(t, W, v1alpha1.StageDraining, func(w *world) { w.autoDrain = false })
	w.now = obj.Status.Deadline.Add(-2 * time.Second)
	res := Step(obj, w.obs(), cfg)
	if res.RequeueAfter != 2*time.Second {
		t.Fatalf("requeue %v", res.RequeueAfter)
	}
}

func TestMirrorDuringEpisode(t *testing.T) {
	w, obj, cfg := at(t, W, v1alpha1.StageDraining, func(w *world) { w.autoDrain = false })
	sm := w.mounts["pv-b"]
	if sm.Status.Remediation == nil || sm.Status.Remediation.Stage != v1alpha1.StageDraining || sm.Status.Remediation.Name != obj.Name {
		t.Fatalf("mirror %+v", sm.Status.Remediation)
	}
	if c := findCondition(sm.Status.Conditions, v1alpha1.ConditionRemediating); c == nil || c.Status != "True" {
		t.Fatalf("Remediating condition missing")
	}
	// Converged mirror: no patch while waiting.
	res := Step(obj, w.obs(), cfg)
	if n := len(cmdsOf[command.PatchSmbMountRemediationMirror](res.Commands)); n != 0 {
		t.Fatalf("%d redundant mirror patches", n)
	}
}
