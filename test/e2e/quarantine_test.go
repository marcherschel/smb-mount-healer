//go:build e2e

package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// quarantineScanWindow bounds "within one scan": QUARANTINE_SCAN_INTERVAL
// (10s in values-e2e.yaml) plus the time to observe the write.
const quarantineScanWindow = 25 * time.Second

// TestS17QuarantineLifecycle: after an S6 sweep quarantined 4 files, the
// SmbQuarantine follows the folder on disk:
//
//	(a) half the files are removed: files drops on the next scan;
//	(b) the SmbQuarantine is deleted while data remains: it is recreated;
//	(c) the folder is moved out of QUARANTINE_DIR: the object is deleted
//	    within one scan interval;
//	(d) a bash-healer style "<pv>-<ts>" folder appears: it is adopted with
//	    an empty remediationRef.
//
// quarantines{pv} goes 1 -> 1 -> 0 -> 1.
func TestS17QuarantineLifecycle(t *testing.T) {
	f := framework.New(t, framework.ClearQuarantines())
	g := f.G
	f.WaitBaseline()
	pv := framework.PV("config-a")
	moved := filepath.Join(framework.KubeletRoot, "smbmh-e2e-moved-quarantine")
	t.Cleanup(func() { f.Chaos.Sh(f.AgentNode, "rm -rf "+framework.ShellQuote(moved)) })

	r := injectShadowFiles(f, pv, "deploy-single", "q1.txt", "q2.txt", "q3.txt", "q4.txt")
	q := checkQuarantined(f, r, pv, 4)
	f.Logf("smbq %s at %s (files %d)", q.Name, q.Spec.Path, q.Status.Files)
	gauge := func() float64 {
		return f.Metrics(metrics.ComponentWorker).Value(metrics.Quarantines, framework.LabelsOf(metrics.LabelPV, pv))
	}
	g.Eventually(gauge).Should(gomega.Equal(1.0), "quarantines{pv} (start)")
	scanned := q.Status.LastScanTime

	// (a) half the files migrated.
	f.Chaos.Sh(f.AgentNode, fmt.Sprintf("rm -f %s %s", framework.ShellQuote(q.Spec.Path+"/q1.txt"), framework.ShellQuote(q.Spec.Path+"/q2.txt")))
	g.Eventually(func(g gomega.Gomega) {
		cur := quarantineAt(f, q.Spec.Path)
		g.Expect(cur).NotTo(gomega.BeNil())
		g.Expect(cur.Status.Files).To(gomega.Equal(int64(2)))
		g.Expect(cur.UID).To(gomega.Equal(q.UID), "same object, updated")
		g.Expect(cur.Status.LastScanTime.After(scanned.Time)).To(gomega.BeTrue(), "rescanned")
	}).WithTimeout(quarantineScanWindow).Should(gomega.Succeed(), "(a) files decrease on the next scan")
	g.Expect(gauge()).To(gomega.Equal(1.0), "quarantines{pv} after (a)")

	// (b) deleting the object does not acknowledge it.
	cur := quarantineAt(f, q.Spec.Path)
	g.Expect(f.Client.Delete(f.Ctx, cur)).To(gomega.Succeed())
	g.Eventually(func(g gomega.Gomega) {
		again := quarantineAt(f, q.Spec.Path)
		g.Expect(again).NotTo(gomega.BeNil())
		g.Expect(again.UID).NotTo(gomega.Equal(cur.UID), "recreated")
		g.Expect(again.Status.Files).To(gomega.Equal(int64(2)))
	}).WithTimeout(quarantineScanWindow).Should(gomega.Succeed(), "(b) smbq recreated while data remains")
	g.Eventually(gauge).Should(gomega.Equal(1.0), "quarantines{pv} after (b)")

	// (c) the data is moved off: the object goes away by itself.
	f.Chaos.Sh(f.AgentNode, fmt.Sprintf("mkdir -p %[1]s && mv %[2]s %[1]s/", framework.ShellQuote(moved), framework.ShellQuote(q.Spec.Path)))
	g.Eventually(func() *v1alpha1.SmbQuarantine { return quarantineAt(f, q.Spec.Path) }).
		WithTimeout(quarantineScanWindow).Should(gomega.BeNil(), "(c) smbq deleted within one scan interval")
	g.Eventually(gauge).Should(gomega.BeZero(), "quarantines{pv} after (c)")
	g.Eventually(func(g gomega.Gomega) {
		if m := f.SmbMount(f.AgentNode, pv); m != nil {
			g.Expect(m.Status.PendingQuarantines).To(gomega.BeZero())
		}
	}).Should(gomega.Succeed(), "pendingQuarantines cleared")

	// (d) a folder left by the bash healer is adopted.
	bash := filepath.Join(framework.QuarantineRoot, pv+"-20250829T101500Z")
	f.Chaos.WriteFile(f.AgentNode, bash+"/left-by-bash.txt", "old shadow data")
	g.Eventually(func(g gomega.Gomega) {
		a := quarantineAt(f, bash)
		g.Expect(a).NotTo(gomega.BeNil())
		g.Expect(a.Spec.RemediationRef).To(gomega.BeEmpty(), "adopted folders have no remediation")
		g.Expect(a.Spec.PVName).To(gomega.Equal(pv), "pv parsed from the folder name")
		g.Expect(a.Spec.QuarantinedTime.UTC()).To(gomega.Equal(time.Date(2025, 8, 29, 10, 15, 0, 0, time.UTC)))
		g.Expect(a.Status.Files).To(gomega.Equal(int64(1)))
		g.Expect(a.Status.Conditions).To(gomega.ContainElement(gomega.And(
			gomega.HaveField("Type", v1alpha1.ConditionPending), gomega.HaveField("Status", gomega.BeEquivalentTo("True")))))
	}).WithTimeout(quarantineScanWindow).Should(gomega.Succeed(), "(d) bash folder adopted")
	g.Eventually(gauge).Should(gomega.Equal(1.0), "quarantines{pv} after (d)")
}

// quarantineAt returns the SmbQuarantine tracking folder path, or nil.
func quarantineAt(f *framework.F, path string) *v1alpha1.SmbQuarantine {
	f.T.Helper()
	for _, q := range f.Quarantines() {
		if q.Spec.Path == path && q.DeletionTimestamp == nil {
			return q.DeepCopy()
		}
	}
	return nil
}
