package remediation

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

func TestBackoffMath(t *testing.T) {
	cfg := testCfg()
	for k, want := range map[int]time.Duration{
		-1: 0, 0: 0, 1: 5 * time.Minute, 2: 10 * time.Minute, 3: 20 * time.Minute, 4: 40 * time.Minute,
		5: time.Hour, 6: time.Hour, 1000: time.Hour,
	} {
		if got := Backoff(k, cfg); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", k, got, want)
		}
	}
	uncapped := Config{BackoffBase: time.Second}
	if got := Backoff(4, uncapped); got != 8*time.Second {
		t.Errorf("uncapped Backoff(4) = %v", got)
	}
	if got := Backoff(200, uncapped); got <= 0 {
		t.Errorf("overflow: %v", got)
	}
}

func triggerIn(w *world, rems ...v1alpha1.SmbMountRemediation) TriggerInput {
	o := w.obs()
	return TriggerInput{NodeName: testNode, Now: w.now, SmbMounts: o.SmbMounts, Pods: o.Pods, Remediations: rems}
}

func finished(name, node string, at time.Time, dryRun bool, mounts ...string) v1alpha1.SmbMountRemediation {
	t := metav1.NewTime(at)
	return v1alpha1.SmbMountRemediation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SmbMountRemediationSpec{NodeName: node, Type: W, DryRun: dryRun,
			Trigger: v1alpha1.RemediationTrigger{SmbMounts: mounts}},
		Status: v1alpha1.SmbMountRemediationStatus{Stage: v1alpha1.StageDone, StageEnteredTime: &t, Outcome: v1alpha1.OutcomeVerifyFailed},
	}
}

var smA = v1alpha1.SmbMountName(testNode, "pv-a")

func TestTriggerWorkload(t *testing.T) {
	w := newWorld()
	d := Trigger(triggerIn(w), testCfg())
	if d.Create == nil || len(d.Suppressed) != 0 {
		t.Fatalf("decision %+v", d)
	}
	c := d.Create
	if c.Spec.Type != W || !slices.Equal(c.Spec.Trigger.SmbMounts, []string{smA}) ||
		!slices.Equal(c.Spec.Trigger.Reasons, []v1alpha1.TriggerReason{v1alpha1.TriggerWrongFSType}) {
		t.Fatalf("spec %+v", c.Spec)
	}
	if c.Name != v1alpha1.RemediationName(testNode, t0) || c.Labels[v1alpha1.LabelNode] != testNode ||
		!slices.Contains(c.Finalizers, v1alpha1.FinalizerRestoreReplicas) || c.Spec.DryRun {
		t.Fatalf("meta %+v", c.ObjectMeta)
	}
	cfg := testCfg()
	cfg.DryRun = true
	if d := Trigger(triggerIn(w), cfg); d.Create == nil || !d.Create.Spec.DryRun {
		t.Fatalf("dry run not propagated")
	}
}

func TestTriggerIgnoresTerminatingAndMissingPods(t *testing.T) {
	w := newWorld()
	w.pods[0].Terminating = true // app-1
	if d := Trigger(triggerIn(w), testCfg()); d.Create != nil {
		t.Fatalf("terminating pod triggered")
	}
	w.removePod("u-app-1")
	w.mounts["pv-a"].Status.PodMounts = append(w.mounts["pv-a"].Status.PodMounts,
		v1alpha1.PodMountStatus{UID: "ghost", Health: v1alpha1.HealthBroken, Reason: v1alpha1.ReasonStatError})
	// pv-a's globalmount is mounted+Broken but still referenced by co-1: no sweep either.
	if d := Trigger(triggerIn(w), testCfg()); d.Create != nil {
		t.Fatalf("stale pod mount triggered: %+v", d.Create.Spec)
	}
}

func TestTriggerSweep(t *testing.T) {
	w := newWorld()
	w.mounts["pv-a"].Status.PodMounts[0].Health = v1alpha1.HealthHealthy
	addSweepMount(w)
	d := Trigger(triggerIn(w), testCfg())
	if d.Create == nil || d.Create.Spec.Type != S {
		t.Fatalf("decision %+v", d)
	}
	want := []v1alpha1.TriggerReason{v1alpha1.TriggerOrphanGlobalmountBroken, v1alpha1.TriggerOrphanPodDirNotEmpty}
	if !slices.Equal(d.Create.Spec.Trigger.Reasons, want) {
		t.Fatalf("reasons %v", d.Create.Spec.Trigger.Reasons)
	}
	// Other orphan shapes.
	sm := w.mounts["pv-s"]
	sm.Status.Health = v1alpha1.HealthUnknown
	sm.Status.Globalmount = &v1alpha1.GlobalmountStatus{DirExists: true}
	sm.Status.OrphanPodDirs = []v1alpha1.OrphanPodDir{{UID: "x", Path: "/x", Mounted: true}}
	d = Trigger(triggerIn(w), testCfg())
	want = []v1alpha1.TriggerReason{v1alpha1.TriggerGlobalmountNotEmpty, v1alpha1.TriggerOrphanPodDirUnhealthy}
	if d.Create == nil || !slices.Equal(d.Create.Spec.Trigger.Reasons, want) {
		t.Fatalf("decision %+v", d.Create)
	}
	// Workload findings take precedence.
	w.mounts["pv-a"].Status.PodMounts[0].Health = v1alpha1.HealthBroken
	if d := Trigger(triggerIn(w), testCfg()); d.Create.Spec.Type != W || len(d.Create.Spec.Trigger.SmbMounts) != 1 {
		t.Fatalf("decision %+v", d.Create.Spec)
	}
}

func TestTriggerActiveRemediation(t *testing.T) {
	w := newWorld()
	active := finished("old", testNode, t0, false, smA)
	active.Status.Stage = v1alpha1.StageDraining
	d := Trigger(triggerIn(w, active), testCfg())
	if d.Create != nil || d.Suppressed[smA] != v1alpha1.SuppressionActiveRemediation {
		t.Fatalf("decision %+v", d)
	}
}

func TestTriggerScopeConflict(t *testing.T) {
	w := newWorld()
	d := Trigger(triggerIn(w, olderActive("n2", dep("app"))), testCfg())
	if d.Create != nil || d.Suppressed[smA] != v1alpha1.SuppressionScopeConflict {
		t.Fatalf("decision %+v", d)
	}
	d = Trigger(triggerIn(w, olderActive("n2", dep("other"))), testCfg())
	if d.Create == nil {
		t.Fatalf("unrelated scope suppressed")
	}
}

func TestTriggerBackoff(t *testing.T) {
	cfg := testCfg()
	w := newWorld()
	cases := []struct {
		name     string
		history  []v1alpha1.SmbMountRemediation
		suppress bool
	}{
		{"none", nil, false},
		{"one 4m ago", []v1alpha1.SmbMountRemediation{finished("a", testNode, t0.Add(-4*time.Minute), false, smA)}, true},
		{"one 6m ago", []v1alpha1.SmbMountRemediation{finished("a", testNode, t0.Add(-6*time.Minute), false, smA)}, false},
		{"two, last 9m ago", []v1alpha1.SmbMountRemediation{
			finished("a", testNode, t0.Add(-30*time.Minute), false, smA),
			finished("b", testNode, t0.Add(-9*time.Minute), false, smA)}, true},
		{"two, last 11m ago", []v1alpha1.SmbMountRemediation{
			finished("a", testNode, t0.Add(-30*time.Minute), false, smA),
			finished("b", testNode, t0.Add(-11*time.Minute), false, smA)}, false},
		{"three, last 19m ago", []v1alpha1.SmbMountRemediation{
			finished("a", testNode, t0.Add(-50*time.Minute), false, smA),
			finished("b", testNode, t0.Add(-40*time.Minute), false, smA),
			finished("c", testNode, t0.Add(-19*time.Minute), false, smA)}, true},
		{"outside window", []v1alpha1.SmbMountRemediation{
			finished("a", testNode, t0.Add(-61*time.Minute), false, smA),
			finished("b", testNode, t0.Add(-6*time.Minute), false, smA)}, false},
		{"other node", []v1alpha1.SmbMountRemediation{finished("a", "n2", t0.Add(-1*time.Minute), false, v1alpha1.SmbMountName("n2", "pv-a"))}, false},
		{"other mode", []v1alpha1.SmbMountRemediation{finished("a", testNode, t0.Add(-1*time.Minute), true, smA)}, false},
		{"via scope pvs", []v1alpha1.SmbMountRemediation{func() v1alpha1.SmbMountRemediation {
			r := finished("a", testNode, t0.Add(-1*time.Minute), false, v1alpha1.SmbMountName(testNode, "pv-b"))
			r.Status.Scope = &v1alpha1.RemediationScope{PVs: []string{"pv-a"}}
			return r
		}()}, true},
	}
	for _, c := range cases {
		d := Trigger(triggerIn(w, c.history...), cfg)
		if got := d.Suppressed[smA] == v1alpha1.SuppressionBackoff; got != c.suppress || (d.Create == nil) != c.suppress {
			t.Errorf("%s: suppressed=%v create=%v", c.name, got, d.Create != nil)
		}
	}
}
