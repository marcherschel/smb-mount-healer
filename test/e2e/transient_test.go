//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// softGomega evaluates Eventually without failing the test (returns bool).
func softGomega() gomega.Gomega {
	return gomega.NewGomega(func(string, ...int) {})
}

// suspectMounts lists the SmbMounts the recorder saw Suspect (top level or
// on a pod mount).
func suspectMounts(f *framework.F, rec *framework.Recorder) []string {
	var out []string
	for _, m := range f.SmbMounts() {
		if rec.SawHealth(m.Name, v1alpha1.HealthSuspect, true) {
			out = append(out, m.Name)
		}
	}
	return out
}

// brokenMounts lists the SmbMounts the recorder saw Broken.
func brokenMounts(f *framework.F, rec *framework.Recorder) []string {
	var out []string
	for _, m := range f.SmbMounts() {
		if rec.SawHealth(m.Name, v1alpha1.HealthBroken, true) {
			out = append(out, m.Name)
		}
	}
	return out
}

// expectAllHealthy waits until every SmbMount and every pod mount is Healthy.
func expectAllHealthy(f *framework.F) {
	f.T.Helper()
	f.G.Eventually(func(g gomega.Gomega) {
		for _, m := range f.SmbMounts() {
			g.Expect(m.Status.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "smbm %s", m.Name)
			for _, pm := range m.Status.PodMounts {
				g.Expect(pm.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "smbm %s pod %s", m.Name, pm.Pod)
			}
		}
	}).WithTimeout(3*time.Minute).Should(gomega.Succeed(), "all mounts healthy again")
}

// expectNoRemediation asserts that no remediation is created for a while
// and that the control plane started none during the test.
func expectNoRemediation(f *framework.F, cp0 framework.Scrape) {
	f.T.Helper()
	f.G.Consistently(func() int { return len(f.NewRemediations()) }).Should(gomega.BeZero(), "no remediation")
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	f.G.Expect(cp.Value(metrics.RemediationsStartedTotal, nil)).To(gomega.BeZero(), "remediations_started")
}

// TestS2TransientBlip: smbd is frozen for 7s (shorter than
// SLOW_CONFIRM_WINDOW). Mounts go Suspect and come back Healthy, the worker
// counts a false alarm, and no remediation starts. A 7s pause can fall
// between two probe rounds (CHECK_INTERVAL 10s), so the blip is repeated
// (after the previous one ended) until a Suspect state was observed.
func TestS2TransientBlip(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	w0 := f.Metrics(metrics.ComponentWorker)

	soft := softGomega()
	seen := false
	for attempt := 1; attempt <= 5 && !seen; attempt++ {
		f.Logf("blip %d: SIGSTOP smbd for 7s", attempt)
		f.Samba.Pause(7 * time.Second)
		seen = soft.Eventually(func() []string { return suspectMounts(f, rec) }).
			WithTimeout(15 * time.Second).WithPolling(500 * time.Millisecond).Should(gomega.Not(gomega.BeEmpty()))
	}
	g.Expect(seen).To(gomega.BeTrue(), "no SmbMount went Suspect after 5 blips")
	f.Logf("suspect: %v", suspectMounts(f, rec))

	expectAllHealthy(f)
	g.Expect(brokenMounts(f, rec)).To(gomega.BeEmpty(), "a 7s blip must never reach Broken")
	g.Eventually(func() float64 {
		return framework.Delta(w0, f.Metrics(metrics.ComponentWorker)).Value(metrics.FalseAlarmsTotal, nil)
	}).Should(gomega.BeNumerically(">=", 1), "false_alarms")
	expectNoRemediation(f, cp0)
	for _, m := range suspectMounts(f, rec) {
		f.Logf("%s healths: %v", m, rec.Healths(m))
	}
}

// TestS3SlowServer: smbd is frozen for 20s, longer than STAT_TIMEOUT (5s)
// but shorter than SLOW_CONFIRM_WINDOW (60s in values-e2e.yaml). Probes time
// out or hang (stat_timeout / probe_hung), mounts stay Suspect and recover;
// no remediation, and probes_hung returns to 0. The window leaves room for
// the CIFS client's recovery after SIGCONT: on kernel 6.17 one mount kept
// timing out ~20s after smbd resumed (35s of failures in total), which a 30s
// window turned into Broken.
func TestS3SlowServer(t *testing.T) {
	f := framework.New(t)
	slowServer(f, func() {
		f.Logf("SIGSTOP smbd for 20s")
		f.Samba.Pause(20 * time.Second)
	})
}

// TestS3SlowServerNetem: the S3 variant with a 6s tc netem delay on the
// samba pod's veth for 20s. Skipped when sch_netem is unavailable.
func TestS3SlowServerNetem(t *testing.T) {
	f := framework.New(t)
	f.WaitBaseline()
	sp := f.Samba.Pod()
	node := sp.Spec.NodeName
	res, err := f.Sh(framework.SambaNamespace, sp.Name, "samba", "cat /sys/class/net/eth0/iflink")
	f.G.Expect(err).NotTo(gomega.HaveOccurred())
	idx := strings.TrimSpace(res.Stdout)
	dev := strings.TrimSpace(f.Chaos.Sh(node, fmt.Sprintf(`ip -o link | awk -F': ' '$1==%s {print $2}' | cut -d@ -f1`, idx)))
	f.G.Expect(dev).NotTo(gomega.BeEmpty(), "host veth of the samba pod (ifindex %s)", idx)
	// Probe netem support (the kernel autoloads sch_netem if the host has it).
	if out, err := f.Chaos.Exec(node, "sh", "-c", fmt.Sprintf("tc qdisc add dev %[1]s root netem delay 1ms && tc qdisc del dev %[1]s root", dev)); err != nil {
		t.Skipf("sch_netem unavailable on the host kernel (tc on %s: %v %s)", dev, err, out.Stderr)
	}
	slowServer(f, func() {
		f.Logf("netem delay 6s on %s (%s) for 20s", dev, node)
		f.T.Cleanup(func() {
			_, _ = f.Chaos.Exec(node, "sh", "-c", "tc qdisc del dev "+dev+" root 2>/dev/null || true")
		})
		f.Chaos.Sh(node, fmt.Sprintf(
			"tc qdisc add dev %[1]s root netem delay 6s && (setsid sh -c 'sleep 20; tc qdisc del dev %[1]s root' </dev/null >/dev/null 2>&1 &)", dev))
	})
}

func slowServer(f *framework.F, inject func()) {
	f.T.Helper()
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	w0 := f.Metrics(metrics.ComponentWorker)

	inject()
	g.Eventually(func() []string { return suspectMounts(f, rec) }).WithTimeout(time.Minute).
		WithPolling(500*time.Millisecond).ShouldNot(gomega.BeEmpty(), "some SmbMount goes Suspect")
	expectAllHealthy(f)
	g.Expect(brokenMounts(f, rec)).To(gomega.BeEmpty(), "a slow server shorter than SLOW_CONFIRM_WINDOW must never reach Broken")

	g.Eventually(func(g gomega.Gomega) {
		w := f.Metrics(metrics.ComponentWorker)
		d := framework.Delta(w0, w)
		slow := d.Get(metrics.MountChecksTotal, metrics.LabelResult, string(v1alpha1.ReasonStatTimeout)) +
			d.Get(metrics.MountChecksTotal, metrics.LabelResult, string(v1alpha1.ReasonProbeHung))
		g.Expect(slow).To(gomega.BeNumerically(">=", 1), "stat_timeout/probe_hung checks:\n%s", w.Series(metrics.MountChecksTotal))
		g.Expect(w.Value(metrics.ProbesHung, nil)).To(gomega.BeZero(), "probes_hung back to 0")
		g.Expect(d.Value(metrics.FalseAlarmsTotal, nil)).To(gomega.BeNumerically(">=", 1), "false_alarms")
	}).Should(gomega.Succeed())
	expectNoRemediation(f, cp0)
	for _, m := range suspectMounts(f, rec) {
		f.Logf("%s healths: %v", m, rec.Healths(m))
	}
}
