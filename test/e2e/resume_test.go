//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// TestS12ControlPlaneKilled: the control-plane pod is deleted while the
// remediation is in awaiting_unstage_completion. The new leader reloads the
// remediation from its status and finishes it (remediations_resumed).
//
// awaiting_unstage_completion normally lasts about a second: it ends when
// kubelet's NodeUnstageVolume removes the globalmount dir the quarantine just
// emptied. The stage is held without racing that, by gating on what the
// remediation waits for instead of on timing:
//
//  1. the worker is frozen during draining, so no host task can run and the
//     remediation stops at the next host task (releasing_mounts or
//     quarantining), before anything was quarantined;
//  2. the csi-smb node plugin ("smb" container) is frozen, so kubelet cannot
//     unstage the volume and the globalmount dir stays;
//  3. the worker is thawed: it runs the quarantine and keeps reporting the
//     (now empty) globalmount dir, so the stage stays in
//     awaiting_unstage_completion for as long as the plugin is frozen;
//  4. the control plane is replaced, then the plugin is thawed.
//
// The hold is far shorter than QUARANTINE_UNSTAGE_WAIT (300s) and than the
// plugin's liveness probe (5 failures x 30s).
func TestS12ControlPlaneKilled(t *testing.T) {
	f := framework.New(t, framework.ClearQuarantines())
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	pv := framework.PV("config-a")
	worker := workerPod(f)
	plugin := f.ReadyPod(framework.CSINamespace, framework.CSINodeLabel, f.AgentNode)

	at := injectWrongFSType(f, pv, "deploy-single", "shadow.txt")
	r := f.WaitNewRemediation()
	waitStage(f, r.Name, 5*time.Minute, v1alpha1.StageDraining)
	f.Freeze(worker)
	stalled := waitPendingHostTask(f, r.Name, 3*time.Minute)
	f.Logf("worker frozen; remediation %s waits for host task %s in %s", r.Name, stalled.Name, currentStage(f, r.Name))
	for _, ht := range f.HostTasksOf(r.Name) {
		g.Expect(strings.Contains(ht.Name, string(v1alpha1.StageQuarantining)) && ht.Status.Phase == v1alpha1.HostTaskSucceeded).
			To(gomega.BeFalse(), "quarantine task %s ran before the worker was frozen", ht.Name)
	}

	f.FreezeContainer(plugin, framework.CSINodeContainer)
	f.Thaw(worker)
	waitHostTask(f, r.Name, string(v1alpha1.StageQuarantining), v1alpha1.HostTaskSucceeded, 3*time.Minute)
	stage := waitStage(f, r.Name, time.Minute, v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageRestartingDriver,
		v1alpha1.StageScalingUp, v1alpha1.StageVerifying, v1alpha1.StageFinalizing, v1alpha1.StageDone)
	f.Logf("csi plugin frozen; remediation %s is in %s", r.Name, stage)
	g.Expect(stage).To(gomega.Equal(v1alpha1.StageAwaitingUnstageCompletion))
	g.Consistently(func() v1alpha1.Stage { return currentStage(f, r.Name) }).WithTimeout(5*time.Second).
		Should(gomega.Equal(v1alpha1.StageAwaitingUnstageCompletion), "stage held while kubelet cannot unstage")

	old := f.HealerPod(metrics.ComponentControlPlane)
	np := f.Kill(metrics.ComponentControlPlane)
	f.Logf("control plane %s replaced by %s (node %s)", old.Name, np.Name, np.Spec.NodeName)
	f.ThawContainer(plugin, framework.CSINodeContainer)

	done := f.WaitRemediation(v1alpha1.OutcomeHealed)
	g.Expect(done.Name).To(gomega.Equal(r.Name))
	checkHealed(f, rec, done, healExpect{
		Type: v1alpha1.RemediationTypeWorkload, Trace: workloadPath,
		Workloads: []string{"deploy-single"}, PVs: []string{pv}, BrokenPVs: []string{pv},
	}, at)
	checkQuarantined(f, done, pv, 1)
	cp := f.Metrics(metrics.ComponentControlPlane) // the new process
	g.Expect(cp.Value(metrics.RemediationsResumedTotal, framework.LabelsOf(metrics.LabelStage, string(v1alpha1.StageAwaitingUnstageCompletion)))).
		To(gomega.Equal(1.0), "remediations_resumed:\n%s", cp.Series(metrics.RemediationsResumedTotal))
	g.Expect(cp.Value(metrics.RemediationsStartedTotal, nil)).To(gomega.BeZero(), "the new leader started nothing new")
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}

// TestS12aWorkerKilled: the worker is killed while a host task of the
// remediation is pending. The worker is frozen during draining, so the
// releasing_mounts task is created but cannot run; then the frozen worker is
// force-deleted. The replacement restores its debounce state from the
// SmbMounts, finishes the pending task, and no second remediation starts.
func TestS12aWorkerKilled(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	pv := framework.PV("config-a")
	worker := workerPod(f)

	at := injectWrongFSType(f, pv, "deploy-single", "")
	r := f.WaitNewRemediation()
	waitStage(f, r.Name, 5*time.Minute, v1alpha1.StageDraining)
	f.Freeze(worker)
	waitStage(f, r.Name, 3*time.Minute, v1alpha1.StageReleasingMounts)
	var pending *v1alpha1.SmbHostTask
	g.Eventually(func(g gomega.Gomega) {
		for _, t := range f.HostTasksOf(r.Name) {
			if t.Status.Phase == "" || t.Status.Phase == v1alpha1.HostTaskPending {
				pending = t.DeepCopy()
			}
		}
		g.Expect(pending).NotTo(gomega.BeNil(), "a host task waiting for the frozen worker")
	}).Should(gomega.Succeed())
	f.Logf("host task %s pending; killing frozen worker %s", pending.Name, worker.Name)
	np := f.KillNow(metrics.ComponentWorker)

	done := f.WaitRemediation(v1alpha1.OutcomeHealed)
	g.Expect(done.Name).To(gomega.Equal(r.Name))
	checkHealed(f, rec, done, healExpect{
		Type: v1alpha1.RemediationTypeWorkload, Trace: workloadPath,
		Workloads: []string{"deploy-single"}, PVs: []string{pv}, BrokenPVs: []string{pv},
	}, at)
	var task v1alpha1.SmbHostTask
	g.Expect(f.Client.Get(f.Ctx, client.ObjectKey{Name: pending.Name}, &task)).To(gomega.Succeed())
	g.Expect(task.Status.Phase).To(gomega.Equal(v1alpha1.HostTaskSucceeded))
	g.Expect(task.Status.Results).To(gomega.HaveLen(len(task.Spec.Actions)))
	for _, res := range task.Status.Results {
		g.Expect(res.Time.Time).To(gomega.BeTemporally(">=", np.CreationTimestamp.Time), "result %s written by the new worker", res.ID)
	}

	// No duplicate episode: the restarted worker restored debounce state.
	g.Consistently(func() int { return len(f.NewRemediations()) }).WithTimeout(20 * time.Second).Should(gomega.Equal(1))
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(cp.Value(metrics.RemediationsStartedTotal, nil)).To(gomega.Equal(1.0), "remediations_started")
}

// TestS12bRemediationDeleted: the SmbMountRemediation is deleted while
// draining. Its finalizer rolls back: the scaled-down workload is restored
// to its original replicas before the object goes away.
func TestS12bRemediationDeleted(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	pv := framework.PV("config-a")

	at := injectWrongFSType(f, pv, "deploy-single", "")
	r := f.WaitNewRemediation()
	waitStage(f, r.Name, 5*time.Minute, v1alpha1.StageDraining)
	g.Eventually(func() int32 { spec, _, _ := f.Fixtures.Replicas("deploy-single"); return spec }).Should(gomega.BeZero(), "scaled down")
	f.Logf("deleting %s while draining", r.Name)
	g.Expect(f.Client.Delete(f.Ctx, r)).To(gomega.Succeed())

	g.Eventually(func() bool {
		var x v1alpha1.SmbMountRemediation
		return apierrors.IsNotFound(f.Client.Get(f.Ctx, client.ObjectKey{Name: r.Name}, &x))
	}).WithTimeout(3*time.Minute).Should(gomega.BeTrue(), "remediation gone after its finalizer ran")
	g.Eventually(func() int32 { spec, _, _ := f.Fixtures.Replicas("deploy-single"); return spec }).Should(gomega.Equal(int32(1)), "replicas restored")
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(workloadAction(cp, "rollback")).To(gomega.Equal(1.0), "rollback:\n%s", cp.After.Series(metrics.WorkloadActionsTotal))
	g.Expect(f.EventReasons("SmbMountRemediation", r.Name, at)).To(gomega.ContainElement("Warning/RollingBack"))
	// Whatever is still broken is healed by a later episode (baseline wait).
}
