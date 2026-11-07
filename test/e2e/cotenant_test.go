//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

var liveSuccess = []string{metrics.LabelMode, metrics.ModeLive, metrics.LabelResult, metrics.ResultSuccess}

func workloadAction(d framework.MetricDelta, action string) float64 {
	return d.Get(metrics.WorkloadActionsTotal, append([]string{metrics.LabelAction, action}, liveSuccess...)...)
}

// TestS9CoTenantPinning: incident 4. cotenant-a and cotenant-b share the
// library PV. Only cotenant-b's pod gets the bare globalmount, but
// cotenant-a's pod keeps the volume staged, so both workloads must be in
// scope and drained for the volume to be unstaged and staged again.
func TestS9CoTenantPinning(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	pv := framework.PV("library")

	at := injectWrongFSType(f, pv, "cotenant-b", "")
	r := f.WaitRemediation(v1alpha1.OutcomeHealed)
	checkHealed(f, rec, r, healExpect{
		Type: v1alpha1.RemediationTypeWorkload, Trace: workloadPath,
		Workloads: []string{"cotenant-a", "cotenant-b"}, PVs: []string{pv}, BrokenPVs: []string{pv},
	}, at)
	g.Expect(r.Status.Verification).To(gomega.HaveLen(2))
	for _, v := range r.Status.Verification {
		g.Expect(v.Result).To(gomega.Equal(v1alpha1.VerificationVerified), "verification of %s", v.Workload)
	}
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(workloadAction(cp, "scale_down")).To(gomega.Equal(2.0), "scale_down:\n%s", cp.After.Series(metrics.WorkloadActionsTotal))
	g.Expect(workloadAction(cp, "scale_up")).To(gomega.Equal(2.0), "scale_up")
	g.Expect(cp.Get(metrics.WorkloadVerificationsTotal, metrics.LabelResult, string(v1alpha1.VerificationVerified))).To(gomega.Equal(2.0))
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}

// TestS10FluxFighter: S9 while flux-fighter keeps scaling cotenant-a back
// to 1 (a GitOps controller reconciling replicas). The draining stage keeps
// re-asserting replicas 0 (scale_reassert). Once that was observed the
// fighter is switched off (Flux reconciles on an interval, not forever), and
// the episode heals.
func TestS10FluxFighter(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	pv := framework.PV("library")

	f.Fixtures.EnableFighter("deployment/cotenant-a", 1)
	at := injectWrongFSType(f, pv, "cotenant-b", "")
	g.Eventually(func() float64 {
		return workloadAction(framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane)), "scale_reassert")
	}).WithTimeout(5*time.Minute).Should(gomega.BeNumerically(">=", 1), "scale_reassert while the fighter scales cotenant-a up")
	f.Logf("scale_reassert observed; switching the fighter off")
	f.Fixtures.DisableFighter()

	r := f.WaitRemediation(v1alpha1.OutcomeHealed)
	checkHealed(f, rec, r, healExpect{
		Type: v1alpha1.RemediationTypeWorkload, Trace: workloadPath,
		Workloads: []string{"cotenant-a", "cotenant-b"}, PVs: []string{pv}, BrokenPVs: []string{pv},
	}, at)
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(workloadAction(cp, "scale_reassert")).To(gomega.BeNumerically(">=", 1))
	g.Expect(workloadAction(cp, "scale_down")).To(gomega.Equal(2.0))
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}

// TestS15UnownedCoTenant: an unowned (bare) pod also uses library. The
// healer cannot scale it, so it keeps the volume staged: draining times out,
// the unstage wait times out with a Warning naming the pod, and the
// re-created pods get the bare globalmount again: verify_failed. Deleting
// the bare pod (cleanup) lets a later episode heal.
func TestS15UnownedCoTenant(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	pv := framework.PV("library")

	bare := f.StartBarePod("bare-cotenant", pv)
	at := injectWrongFSType(f, pv, "cotenant-b", "")
	r := f.WaitRemediation(v1alpha1.OutcomeVerifyFailed)
	trace := framework.StageTraceOf(r)
	f.Logf("remediation %s trace: %s", r.Name, framework.FormatTrace(trace))
	g.Expect(trace).To(gomega.Equal(workloadPath))
	var scoped []string
	for _, w := range r.Status.Scope.Workloads {
		scoped = append(scoped, w.Name)
	}
	g.Expect(scoped).To(gomega.ConsistOf("cotenant-a", "cotenant-b"), "the bare pod has no owner to scale")
	g.Expect(r.Status.Verification).To(gomega.ContainElement(gomega.HaveField("Result", v1alpha1.VerificationFailed)))

	var pinned string
	for _, e := range f.Events("SmbMountRemediation", r.Name, at) {
		if e.Type == "Warning" && e.Reason == "UnstagePinned" {
			pinned = e.Note
		}
	}
	g.Expect(pinned).To(gomega.ContainSubstring(bare.Name), "UnstagePinned names the bare pod; events: %v",
		f.EventReasons("SmbMountRemediation", r.Name, at))
	g.Expect(f.EventReasons("SmbMountRemediation", r.Name, at)).To(gomega.ContainElements(
		"Warning/DrainTimeout", "Warning/UnstagePinned", "Warning/RemediationFinished"))
	timeouts := map[v1alpha1.Stage]string{
		v1alpha1.StageDraining:                  "scale_down_timeout",
		v1alpha1.StageAwaitingUnstageCompletion: "unstage_timeout",
		v1alpha1.StageVerifying:                 "verify_timeout",
	}
	for _, tr := range r.Status.Transitions {
		if want, ok := timeouts[tr.From]; ok {
			g.Expect(tr.Reason).To(gomega.Equal(want), "exit of %s", tr.From)
		}
	}
	m := f.SmbMount(f.AgentNode, pv)
	g.Expect(m.Status.LastRemediation).NotTo(gomega.BeNil())
	g.Expect(m.Status.LastRemediation.Outcome).To(gomega.Equal(v1alpha1.OutcomeVerifyFailed))

	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(cp.Get(metrics.RemediationsTotal, metrics.LabelOutcome, string(v1alpha1.OutcomeVerifyFailed))).To(gomega.Equal(1.0))
	g.Expect(cp.Get(metrics.WorkloadVerificationsTotal, metrics.LabelResult, string(v1alpha1.VerificationFailed))).To(gomega.BeNumerically(">=", 1))
	g.Expect(strings.Join(scoped, ",")).NotTo(gomega.ContainSubstring(bare.Name))

	// Recovery (the bare pod gone, a later episode after backoff) is left to
	// the baseline wait; it must not take the replicas of the fixtures down
	// for good.
	f.DeleteBarePod(bare.Name)
}
