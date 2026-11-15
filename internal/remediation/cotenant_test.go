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
