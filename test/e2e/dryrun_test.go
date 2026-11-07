//go:build e2e

package e2e

import (
	"testing"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// TestS16DryRun: with the control plane in --dry-run (DRY_RUN=true), S4
// produces a remediation that walks every stage with outcome dry_run: host
// tasks are dry-run and only report what they would do, no workload is
// scaled, the CSI driver pod is not restarted and nothing is quarantined.
// Switching back to live mode (cleanup) lets a live episode heal the mount.
// The smb_mount_healer_dry_run gauge follows the mode (1 dry run, 0 live).
func TestS16DryRun(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	f.SetControlPlaneDryRun(true)
	t.Cleanup(func() { f.SetControlPlaneDryRun(false) })
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	w0 := f.Metrics(metrics.ComponentWorker)
	cpLabels := framework.Labels{metrics.LabelComponent: metrics.ComponentControlPlane}
	g.Expect(cp0.Has(metrics.DryRun, cpLabels)).To(gomega.BeTrue(), "dry_run gauge:\n%s", cp0.Series(metrics.DryRun))
	g.Expect(cp0.Value(metrics.DryRun, cpLabels)).To(gomega.Equal(1.0), "dry_run gauge:\n%s", cp0.Series(metrics.DryRun))
	driver := f.ReadyPod(framework.CSINamespace, framework.CSINodeLabel, f.AgentNode)
	pv := framework.PV("config-a")

	injectWrongFSType(f, pv, "deploy-single", "")
	r := f.WaitRemediation(v1alpha1.OutcomeDryRun)
	trace := framework.StageTraceOf(r)
	f.Logf("remediation %s trace: %s", r.Name, framework.FormatTrace(trace))
	g.Expect(r.Spec.DryRun).To(gomega.BeTrue())
	g.Expect(trace).To(gomega.Equal(workloadPath))
	// Wait guards pass immediately in dry run (stages without work may
	// still exit with nothing_to_do).
	waits := map[v1alpha1.Stage]bool{
		v1alpha1.StageDraining: true, v1alpha1.StageAwaitingUnstageCompletion: true, v1alpha1.StageRestartingDriver: true,
		v1alpha1.StageScalingUp: true, v1alpha1.StageVerifying: true,
	}
	for _, tr := range r.Status.Transitions {
		if waits[tr.From] {
			g.Expect(tr.Reason).To(gomega.Equal("dry_run"), "transition %s -> %s", tr.From, tr.To)
		}
	}

	tasks := f.HostTasksOf(r.Name)
	g.Expect(tasks).NotTo(gomega.BeEmpty())
	for _, task := range tasks {
		g.Expect(task.Spec.DryRun).To(gomega.BeTrue(), "host task %s dryRun", task.Name)
	}
	g.Eventually(func(g gomega.Gomega) {
		for typ, results := range hostResults(f, r.Name) {
			for _, res := range results {
				g.Expect(res.Result).To(gomega.BeElementOf(v1alpha1.HostActionDryRun, v1alpha1.HostActionSkipped),
					"%s result %s: nothing may be changed in dry run", typ, res.ID)
			}
		}
	}).Should(gomega.Succeed())

	// Nothing changed in the cluster or on the host.
	spec, _, err := f.Fixtures.Replicas("deploy-single")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(spec).To(gomega.Equal(int32(1)), "replicas unchanged")
	g.Expect(f.ReadyPod(framework.CSINamespace, framework.CSINodeLabel, f.AgentNode).UID).To(gomega.Equal(driver.UID), "driver pod not restarted")
	g.Expect(f.Quarantines()).To(gomega.BeEmpty(), "no smbq in dry run")
	m := f.SmbMount(f.AgentNode, pv)
	g.Expect(m.Status.LastRemediation).NotTo(gomega.BeNil())
	g.Expect(m.Status.LastRemediation.Outcome).To(gomega.Equal(v1alpha1.OutcomeDryRun))

	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	w := framework.Delta(w0, f.Metrics(metrics.ComponentWorker))
	g.Expect(cp.Get(metrics.WorkloadActionsTotal, metrics.LabelMode, metrics.ModeLive)).To(gomega.BeZero(),
		"workload_actions{mode=live}:\n%s", cp.After.Series(metrics.WorkloadActionsTotal))
	g.Expect(cp.Get(metrics.RemediationsStartedTotal, metrics.LabelMode, metrics.ModeDryRun)).To(gomega.BeNumerically(">=", 1))
	g.Expect(cp.Get(metrics.RemediationsTotal, metrics.LabelMode, metrics.ModeDryRun, metrics.LabelOutcome, string(v1alpha1.OutcomeDryRun))).To(gomega.BeNumerically(">=", 1))
	g.Expect(w.Get(metrics.HostActionsTotal, metrics.LabelDryRun, "true", metrics.LabelResult, "dry_run")).To(gomega.BeNumerically(">=", 1),
		"host_actions{dry_run}:\n%s", w.After.Series(metrics.HostActionsTotal))
	g.Expect(w.Get(metrics.HostActionsTotal, metrics.LabelDryRun, "true", metrics.LabelResult, "succeeded")).To(gomega.BeZero())

	// Back to live mode (the cleanup repeats this idempotently): the gauge drops to 0.
	f.SetControlPlaneDryRun(false)
	live := f.Metrics(metrics.ComponentControlPlane)
	g.Expect(live.Has(metrics.DryRun, cpLabels)).To(gomega.BeTrue(), "dry_run gauge:\n%s", live.Series(metrics.DryRun))
	g.Expect(live.Value(metrics.DryRun, cpLabels)).To(gomega.BeZero(), "dry_run gauge:\n%s", live.Series(metrics.DryRun))
}
