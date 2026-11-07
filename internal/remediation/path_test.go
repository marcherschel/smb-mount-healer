package remediation

import (
	"slices"
	"testing"
	"time"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
)

func TestWorkloadHappyPath(t *testing.T) {
	w := newWorld()
	obj := workloadRem()
	cfg := testCfg()
	var actions []command.WorkloadAction
	for i := 0; i < 200 && !obj.Status.Stage.Terminal(); i++ {
		res := w.step(t, obj, cfg)
		for _, s := range cmdsOf[command.ScaleWorkload](res.Commands) {
			actions = append(actions, s.Action)
		}
		if len(cmdsOf[command.DeletePod](res.Commands)) > 0 {
			actions = append(actions, command.ActionDriverRestart)
		}
		w.now = w.now.Add(3 * time.Second)
	}
	if obj.Status.Outcome != v1alpha1.OutcomeHealed {
		t.Fatalf("outcome %q, trace %s", obj.Status.Outcome, trace(obj))
	}
	want := []string{"", "pending", "planning", "draining", "releasing_mounts", "awaiting_unstage", "quarantining",
		"awaiting_unstage_completion", "restarting_driver", "scaling_up", "verifying", "finalizing"}
	var got []string
	for _, tr := range obj.Status.Transitions {
		got = append(got, string(tr.From))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("path %v\nwant %v\ntrace %s", got, want, trace(obj))
	}
	// Scope: app (broken) + co (co-tenant of the dead pv-a globalmount), not other (healthy pv-b).
	var ws []string
	for _, sw := range obj.Status.Scope.Workloads {
		ws = append(ws, sw.Name+"/"+string(sw.State))
		if sw.Name == "app" && sw.OriginalReplicas != 2 {
			t.Fatalf("app originalReplicas %d", sw.OriginalReplicas)
		}
	}
	if !slices.Equal(ws, []string{"app/restored", "co/restored"}) {
		t.Fatalf("scope workloads %v", ws)
	}
	if !slices.Equal(obj.Status.Scope.PVs, []string{"pv-a", "pv-b"}) {
		t.Fatalf("scope pvs %v", obj.Status.Scope.PVs)
	}
	if w.workloads[dep("app").String()].Replicas != 2 || w.workloads[dep("other").String()].Replicas != 1 {
		t.Fatalf("replicas not restored")
	}
	if obj.Status.Driver == nil || obj.Status.Driver.OldPodUID != "drv-1" || obj.Status.Driver.NewPodUID != "drv-2" {
		t.Fatalf("driver %+v", obj.Status.Driver)
	}
	for _, v := range obj.Status.Verification {
		if v.Result != v1alpha1.VerificationVerified {
			t.Fatalf("verification %+v", obj.Status.Verification)
		}
	}
	if len(obj.Status.DeadGlobalmounts) != 1 || obj.Status.DeadGlobalmounts[0].PVName != "pv-a" {
		t.Fatalf("dead gms %+v", obj.Status.DeadGlobalmounts)
	}
	if !slices.Contains(actions, command.ActionScaleDown) || !slices.Contains(actions, command.ActionScaleUp) ||
		!slices.Contains(actions, command.ActionDriverRestart) {
		t.Fatalf("actions %v", actions)
	}
	// Mirror converged: lastRemediation on both pv mounts, no active remediation.
	for _, pv := range []string{"pv-a", "pv-b"} {
		sm := w.mounts[pv]
		if sm.Status.Remediation != nil || sm.Status.LastRemediation == nil ||
			sm.Status.LastRemediation.Outcome != v1alpha1.OutcomeHealed {
			t.Fatalf("mirror on %s: %+v %+v", pv, sm.Status.Remediation, sm.Status.LastRemediation)
		}
	}
	// Terminal and converged: no more commands, no requeue.
	res := Step(obj, w.obs(), cfg)
	if len(res.Commands) != 0 || res.RequeueAfter != 0 || len(res.Transitions) != 0 {
		t.Fatalf("terminal step not quiet: %v", keys(res.Commands))
	}
}
