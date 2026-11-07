//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// sambaWorkloads are the fixture workloads on the samba server (not the landmine).
func sambaWorkloads() []string {
	var out []string
	for _, w := range framework.Workloads {
		if w.Replicas > 0 && len(w.Volumes) > 0 && w.Volumes[0].Server == "samba" {
			out = append(out, w.Name)
		}
	}
	return out
}

// heartbeatsSince reports whether every samba workload's heartbeat on the
// server side is newer than t (the kernel client reconnected and writes
// reach the server again).
func heartbeatsSince(f *framework.F, t time.Time) bool {
	for _, w := range sambaWorkloads() {
		hb, err := f.Fixtures.Heartbeat(w)
		if err != nil {
			return false
		}
		for _, v := range hb {
			if epoch(v) <= t.Unix() {
				return false
			}
		}
	}
	return true
}

// TestS13SambaNewIP: the samba pod is deleted; its replacement has a new
// pod IP behind the same DNS name. If the kernel cifs client re-resolves
// the name and reconnects by itself, there is nothing to heal (skipped).
// Otherwise the mounts fail (stat_error / stat_timeout: the old IP is gone)
// and remediations heal every affected workload, each SmbMount at most once.
func TestS13SambaNewIP(t *testing.T) {
	f := framework.New(t, framework.ClearQuarantines())
	g := f.G
	f.WaitBaseline()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	oldIP := f.Samba.Pod().Status.PodIP

	np := f.Samba.Restart()
	restarted := time.Now()
	f.Logf("samba restarted: %s -> %s", oldIP, np.Status.PodIP)
	if np.Status.PodIP == oldIP {
		t.Skipf("samba came back with the same pod IP %s; the scenario needs a new one", oldIP)
	}

	var outcome string
	g.Eventually(func() string {
		switch {
		case len(f.NewRemediations()) > 0:
			outcome = "remediation"
		case heartbeatsSince(f, restarted.Add(5*time.Second)):
			outcome = "self-healed"
		}
		return outcome
	}).WithTimeout(4*time.Minute).ShouldNot(gomega.BeEmpty(), "either a remediation starts or the kernel reconnects")
	if outcome == "self-healed" {
		g.Consistently(func() int { return len(f.NewRemediations()) }).Should(gomega.BeZero())
		t.Skip("the kernel cifs client reconnected to the new samba IP by itself; nothing to remediate")
	}

	// Mounts reach Broken at slightly different times (stat_timeout is a
	// slow class: each mount needs SLOW_CONFIRM_WINDOW from its own first
	// failure), and a remediation's scope is fixed by its trigger. So the
	// mounts broken when the first episode starts are healed by it, and
	// the ones that cross the window while it runs get the next episode on
	// this node (backoff is per PV and does not apply to them). Every
	// episode must heal, and no SmbMount may trigger twice.
	g.Eventually(func(g gomega.Gomega) {
		rs := f.NewRemediations()
		g.Expect(rs).NotTo(gomega.BeEmpty())
		for _, r := range rs {
			g.Expect(r.Status.Stage).To(gomega.Equal(v1alpha1.StageDone), "remediation %s", r.Name)
			if r.Status.Outcome != v1alpha1.OutcomeHealed {
				gomega.StopTrying("remediation " + r.Name + " finished " + string(r.Status.Outcome)).Now()
			}
		}
		for _, m := range f.SmbMounts() {
			g.Expect(m.Status.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "smbm %s", m.Name)
		}
	}).WithTimeout(framework.RemediationTimeout).Should(gomega.Succeed(), "every episode healed")
	g.Consistently(func() int { return len(f.NewRemediations()) }).Should(gomega.Equal(len(f.NewRemediations())), "no further episodes")
	rs := f.NewRemediations()
	g.Expect(len(rs)).To(gomega.BeNumerically("<=", len(framework.StagedPVs())))
	triggered := map[string]string{}
	for _, r := range rs {
		f.Logf("remediation %s trace %s, trigger %v %v", r.Name, framework.FormatTrace(framework.StageTraceOf(&r)),
			r.Spec.Trigger.Reasons, r.Spec.Trigger.SmbMounts)
		g.Expect(r.Spec.Type).To(gomega.Equal(v1alpha1.RemediationTypeWorkload))
		g.Expect(r.Spec.Trigger.Reasons).To(gomega.ContainElement(gomega.BeElementOf(
			v1alpha1.TriggerStatError, v1alpha1.TriggerStatTimeout, v1alpha1.TriggerProbeHung)))
		for _, sm := range r.Spec.Trigger.SmbMounts {
			g.Expect(triggered).NotTo(gomega.HaveKey(sm), "%s triggered by %s and %s", sm, triggered[sm], r.Name)
			triggered[sm] = r.Name
		}
	}
	for _, w := range sambaWorkloads() {
		checkWorkloadHealthy(f, w, rs[0].CreationTimestamp.Time)
	}
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(cp.Value(metrics.RemediationsStartedTotal, nil)).To(gomega.Equal(float64(len(rs))))
	g.Expect(cp.Get(metrics.RemediationsTotal, metrics.LabelOutcome, string(v1alpha1.OutcomeHealed))).To(gomega.Equal(float64(len(rs))))
}

// TestS14OutageDuringEpisode: samba is scaled to 0 for longer than an
// episode (until it finished; about 2-3m with the e2e waits). The
// mounts break, a remediation drains and scales back up, but the new pods
// cannot mount: verify_failed. While samba is back and kubelet recovers the
// pods, backoff suppresses a second remediation.
func TestS14OutageDuringEpisode(t *testing.T) {
	f := framework.New(t, framework.ClearQuarantines())
	g := f.G
	f.WaitBaseline()
	cp0 := f.Metrics(metrics.ComponentControlPlane)

	t.Cleanup(func() { f.Samba.Scale(1) })
	f.Samba.Scale(0)
	down := time.Now()
	f.Logf("samba scaled to 0")

	r := f.WaitRemediation(v1alpha1.OutcomeVerifyFailed)
	f.Logf("remediation %s trace %s after %s", r.Name, framework.FormatTrace(framework.StageTraceOf(r)), time.Since(down).Round(time.Second))
	g.Expect(r.Status.Verification).To(gomega.ContainElement(gomega.HaveField("Result", v1alpha1.VerificationFailed)))
	// The plan's "4m" outage is just "longer than the episode": samba stays
	// down until the episode ended verify_failed, then comes back.
	f.Samba.Scale(1)
	f.Logf("samba back after %s", time.Since(down).Round(time.Second))

	g.Eventually(func() float64 {
		return framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane)).
			Get(metrics.RemediationSuppressedTotal, metrics.LabelReason, string(v1alpha1.SuppressionBackoff))
	}).WithTimeout(5*time.Minute).Should(gomega.BeNumerically(">=", 1), "remediation_suppressed{backoff}")
	f.WaitBaseline()
	for _, x := range f.NewRemediations() {
		f.Logf("remediation %s: %s outcome %s", x.Name, framework.FormatTrace(framework.StageTraceOf(&x)), x.Status.Outcome)
	}
}
