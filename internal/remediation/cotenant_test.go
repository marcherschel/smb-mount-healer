package remediation

import (
	"testing"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// TestCotenantsStayWhenFlappingGlobalmountProbesHealthy: e2e S3 netem (run
// 43713887810). The globalmount of pv-a is Broken when the remediation plans
// its scope, then a single good probe reports it Healthy before planning
// ends (a share that keeps flapping). The co-tenant co must still be
// drained: otherwise its pod keeps the globalmount pinned, kubelet never
// unstages it, and the replacement of app gets the same bad mount
// (verify_failed).
func TestCotenantsStayWhenFlappingGlobalmountProbesHealthy(t *testing.T) {
	cfg := testCfg()
	w := newWorld()
	obj := workloadRem()

	w.runUntil(t, obj, cfg, v1alpha1.StagePlanning)
	if got := obj.Status.Scope.CotenantPVs; len(got) != 1 || got[0] != "pv-a" {
		t.Fatalf("cotenantPVs at planning entry = %v, want [pv-a]", got)
	}

	// The planning probe catches a good moment of the flapping share.
	w.mounts["pv-a"].Status.Health = v1alpha1.HealthHealthy
	w.runUntil(t, obj, cfg, v1alpha1.StageDone)

	if !scoped(obj, "co") {
		t.Fatalf("co-tenant co dropped from the scope after a healthy probe: %+v", obj.Status.Scope.Workloads)
	}
	if got := obj.Status.Scope.CotenantPVs; len(got) != 1 || got[0] != "pv-a" {
		t.Fatalf("cotenantPVs = %v, want [pv-a] (the set only grows)", got)
	}
	drained := false
	for _, p := range obj.Status.Scope.DrainPods {
		if p.UID == "u-co-1" {
			drained = true
		}
	}
	if !drained {
		t.Fatalf("co-1 not in drainPods: %+v", obj.Status.Scope.DrainPods)
	}
}

// TestCotenantsJoinWhenGlobalmountFailsWhileDraining: the globalmount looked
// healthy during planning (only the pod mount of app was broken, so co is
// not a co-tenant), then fails while app drains. extendScope adds co and
// records pv-a; a later healthy probe does not remove it again.
func TestCotenantsJoinWhenGlobalmountFailsWhileDraining(t *testing.T) {
	cfg := testCfg()
	w := newWorld()
	w.autoDrain = false // keep draining open
	w.mounts["pv-a"].Status.Health = v1alpha1.HealthHealthy
	obj := workloadRem()

	w.runUntil(t, obj, cfg, v1alpha1.StageDraining)
	if scoped(obj, "co") || len(obj.Status.Scope.CotenantPVs) != 0 {
		t.Fatalf("co must not be in scope while pv-a's globalmount is healthy: %+v", obj.Status.Scope)
	}

	w.mounts["pv-a"].Status.Health = v1alpha1.HealthSuspect
	w.step(t, obj, cfg)
	w.mounts["pv-a"].Status.Health = v1alpha1.HealthHealthy
	w.now = w.now.Add(cfg.PollInterval)
	w.step(t, obj, cfg)

	if !scoped(obj, "co") {
		t.Fatalf("co not added while pv-a's globalmount was not healthy: %+v", obj.Status.Scope.Workloads)
	}
	if got := obj.Status.Scope.CotenantPVs; len(got) != 1 || got[0] != "pv-a" {
		t.Fatalf("cotenantPVs = %v, want [pv-a]", got)
	}
}

func scoped(obj *v1alpha1.SmbMountRemediation, workload string) bool {
	if obj.Status.Scope == nil {
		return false
	}
	for _, sw := range obj.Status.Scope.Workloads {
		if sw.WorkloadRef == dep(workload) {
			return true
		}
	}
	return false
}

// TestFlappingGlobalmountIsForceUnmounted: e2e S3 netem (run 43725390878).
// The globalmount of pv-a was Broken when the remediation planned, then
// probes Healthy for a moment. It still counts as dead, and its
// force-unmount carries no probe precondition (every pod using pv-a is
// drained), so one good probe cannot keep the old mount: before, the
// remediation restarted the driver and scaled up onto the same mount.
func TestFlappingGlobalmountIsForceUnmounted(t *testing.T) {
	cfg := testCfg()
	w := newWorld()
	w.stuck = true // kubelet does not unstage on its own
	obj := workloadRem()

	w.runUntil(t, obj, cfg, v1alpha1.StagePlanning)
	w.mounts["pv-a"].Status.Health = v1alpha1.HealthHealthy
	w.runUntil(t, obj, cfg, v1alpha1.StageQuarantining)

	if len(obj.Status.DeadGlobalmounts) != 1 || obj.Status.DeadGlobalmounts[0].PVName != "pv-a" {
		t.Fatalf("deadGlobalmounts = %+v, want pv-a (seen not healthy by this remediation)", obj.Status.DeadGlobalmounts)
	}
	a := forceUnmountOf(t, w, obj, gmPath("pv-a"))
	if a.OnlyIf != "" {
		t.Fatalf("force-unmount of pv-a's globalmount has onlyIf %q, want none (all its pods are drained)", a.OnlyIf)
	}
	if w.mounts["pv-a"].Status.Globalmount.Mounted {
		t.Fatalf("pv-a's globalmount still mounted after the force-unmount")
	}
}

// TestDeadGlobalmountKeepsGuardForUndrainedPod: a pod the remediation does
// not drain (unowned) still uses pv-a, so it still needs the mount; the
// worker's NotHealthyMount check stays and a healthy probe skips it.
func TestDeadGlobalmountKeepsGuardForUndrainedPod(t *testing.T) {
	cfg := testCfg()
	w := newWorld()
	w.stuck = true
	w.addPod("", "bare", "u-bare", map[string]v1alpha1.Health{"pv-a": v1alpha1.HealthHealthy})
	obj := workloadRem()

	w.runUntil(t, obj, cfg, v1alpha1.StagePlanning)
	w.mounts["pv-a"].Status.Health = v1alpha1.HealthHealthy
	w.runUntil(t, obj, cfg, v1alpha1.StageQuarantining)

	a := forceUnmountOf(t, w, obj, gmPath("pv-a"))
	if a.OnlyIf != v1alpha1.OnlyIfNotHealthyMount {
		t.Fatalf("force-unmount of pv-a's globalmount has onlyIf %q, want %s (pod bare still uses it)", a.OnlyIf, v1alpha1.OnlyIfNotHealthyMount)
	}
	if !w.mounts["pv-a"].Status.Globalmount.Mounted {
		t.Fatalf("pv-a's globalmount was unmounted although it probes healthy and pod bare uses it")
	}
}

// forceUnmountOf returns the ForceUnmount action of the awaiting_unstage
// host task for path.
func forceUnmountOf(t *testing.T, w *world, obj *v1alpha1.SmbMountRemediation, path string) v1alpha1.HostAction {
	t.Helper()
	task := w.tasks[v1alpha1.HostTaskName(obj.Name, v1alpha1.StageAwaitingUnstage, 0)]
	if task == nil {
		t.Fatalf("no awaiting_unstage host task; trace %s", trace(obj))
	}
	for _, a := range task.Spec.Actions {
		if a.Type == v1alpha1.HostActionForceUnmount && a.Path == path {
			return a
		}
	}
	t.Fatalf("awaiting_unstage task has no ForceUnmount of %s: %+v", path, task.Spec.Actions)
	return v1alpha1.HostAction{}
}
