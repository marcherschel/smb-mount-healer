//go:build e2e

package e2e

import (
	"testing"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// runWrongFSType is S4/S5/S11: detach pv's globalmount and delete the pods
// of workload; the replacement pod gets the bare local dir (wrong_fstype),
// and a Workload remediation walks the full path to healed.
func runWrongFSType(t *testing.T, workload, pv string, scopePVs []string) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	w0 := f.Metrics(metrics.ComponentWorker)

	at := injectWrongFSType(f, pv, workload, "")
	r := f.WaitRemediation(v1alpha1.OutcomeHealed)
	checkHealed(f, rec, r, healExpect{
		Type:      v1alpha1.RemediationTypeWorkload,
		Trace:     workloadPath,
		Workloads: []string{workload},
		PVs:       scopePVs,
		BrokenPVs: []string{pv},
	}, at)
	g.Expect(r.Spec.Trigger.Reasons).To(gomega.ContainElement(v1alpha1.TriggerWrongFSType), "trigger reasons")
	g.Expect(r.Spec.Trigger.SmbMounts).To(gomega.ContainElement(v1alpha1.SmbMountName(f.AgentNode, pv)))
	g.Expect(r.Status.Driver).NotTo(gomega.BeNil())
	g.Expect(r.Status.Driver.OldPodUID).NotTo(gomega.BeEmpty())
	g.Expect(r.Status.Driver.NewPodUID).NotTo(gomega.BeEmpty())
	g.Expect(r.Status.Driver.NewPodUID).NotTo(gomega.Equal(r.Status.Driver.OldPodUID))
	g.Expect(r.Status.Verification).To(gomega.ConsistOf(gomega.HaveField("Result", v1alpha1.VerificationVerified)))

	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	w := framework.Delta(w0, f.Metrics(metrics.ComponentWorker))
	live := []string{metrics.LabelMode, metrics.ModeLive, metrics.LabelResult, metrics.ResultSuccess}
	g.Expect(cp.Get(metrics.RemediationsStartedTotal, metrics.LabelType, string(v1alpha1.RemediationTypeWorkload))).To(gomega.Equal(1.0))
	g.Expect(cp.Get(metrics.RemediationsTotal, metrics.LabelOutcome, string(v1alpha1.OutcomeHealed))).To(gomega.Equal(1.0))
	g.Expect(cp.Get(metrics.WorkloadActionsTotal, append([]string{metrics.LabelAction, "scale_down"}, live...)...)).To(gomega.Equal(1.0), "scale_down")
	g.Expect(cp.Get(metrics.WorkloadActionsTotal, append([]string{metrics.LabelAction, "scale_up"}, live...)...)).To(gomega.Equal(1.0), "scale_up")
	g.Expect(cp.Get(metrics.WorkloadActionsTotal, append([]string{metrics.LabelAction, "driver_restart"}, live...)...)).To(gomega.Equal(1.0), "driver_restart")
	g.Expect(cp.Get(metrics.WorkloadVerificationsTotal, metrics.LabelResult, string(v1alpha1.VerificationVerified))).To(gomega.Equal(1.0))
	g.Expect(w.Get(metrics.HostActionsTotal, metrics.LabelAction, "quarantine", metrics.LabelDryRun, "false")).To(gomega.BeNumerically(">=", 1),
		"host_actions{quarantine}:\n%s", w.After.Series(metrics.HostActionsTotal))
	g.Expect(w.Get(metrics.MountChecksTotal, metrics.LabelResult, string(v1alpha1.ReasonWrongFSType))).To(gomega.BeNumerically(">=", 1))

	// Exactly one episode.
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}

// TestS4WrongFSType: incident 1. The globalmount is detached and the pod
// recreated; kubelet bind-mounts the bare globalmount dir (wrong_fstype). A
// Workload remediation drains deploy-single, releases and quarantines,
// restarts the CSI node driver, scales back up and verifies cifs.
func TestS4WrongFSType(t *testing.T) {
	runWrongFSType(t, "deploy-single", framework.PV("config-a"), []string{framework.PV("config-a")})
}

// TestS5StatefulSet: S4 on a StatefulSet (scale subresource of apps/v1 StatefulSet).
func TestS5StatefulSet(t *testing.T) {
	runWrongFSType(t, "sts-single", framework.PV("config-b"), []string{framework.PV("config-b")})
}

// TestS11TwoVolumePod: only the dl globalmount of two-vol (config-c + dl,
// the transmission shape) is detached; the scope covers both PVs because the
// pod has to be drained off both.
func TestS11TwoVolumePod(t *testing.T) {
	runWrongFSType(t, "two-vol", framework.PV("dl"), []string{framework.PV("config-c"), framework.PV("dl")})
}
