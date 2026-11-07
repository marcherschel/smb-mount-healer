//go:build e2e

package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/uuid"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// checkSweep asserts the common Sweep expectations for remediation r.
func checkSweep(f *framework.F, r *v1alpha1.SmbMountRemediation, pv string, reason v1alpha1.TriggerReason) {
	f.T.Helper()
	g := f.G
	trace := framework.StageTraceOf(r)
	f.Logf("remediation %s trace: %s", r.Name, framework.FormatTrace(trace))
	g.Expect(r.Spec.Type).To(gomega.Equal(v1alpha1.RemediationTypeSweep))
	g.Expect(r.Status.Outcome).To(gomega.Equal(v1alpha1.OutcomeHealed))
	g.Expect(trace).To(gomega.Equal(sweepPath), "stage trace %s", framework.FormatTrace(trace))
	g.Expect(r.Spec.Trigger.SmbMounts).To(gomega.ConsistOf(v1alpha1.SmbMountName(f.AgentNode, pv)))
	g.Expect(r.Spec.Trigger.Reasons).To(gomega.ContainElement(reason))
	g.Expect(r.Status.Scope).NotTo(gomega.BeNil())
	g.Expect(r.Status.Scope.PVs).To(gomega.ConsistOf(pv))
	g.Expect(r.Status.Scope.Workloads).To(gomega.BeEmpty(), "a sweep never touches workloads")
	tasks := f.HostTasksOf(r.Name)
	g.Expect(tasks).NotTo(gomega.BeEmpty())
	for _, t := range tasks {
		g.Expect(t.Status.Phase).To(gomega.Equal(v1alpha1.HostTaskSucceeded), "host task %s: %+v", t.Name, t.Status.Results)
	}
	g.Expect(f.EventReasons("SmbMountRemediation", r.Name, r.CreationTimestamp.Time)).To(
		gomega.ContainElement("Normal/RemediationFinished"))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// injectShadowFiles is the incident-2 injection: detach pv's globalmount,
// write files into the bare directory and scale workload (the PV's only
// user) to 0, so kubelet cannot unstage and nothing references the PV. It
// waits for the resulting Sweep to heal and returns it. Registers scaling
// the workload back up as a cleanup.
func injectShadowFiles(f *framework.F, pv, workload string, files ...string) *v1alpha1.SmbMountRemediation {
	f.T.Helper()
	f.Logf("inject: detach %s, seed %d shadow file(s), scale %s to 0", pv, len(files), workload)
	f.Fixtures.DetachGlobalmount(pv)
	for _, name := range files {
		f.Chaos.WriteFile(f.AgentNode, filepath.Join(f.Fixtures.GlobalmountPath(pv), name), "shadow data written while the share was not mounted")
	}
	f.T.Cleanup(func() { f.Fixtures.Scale(workload, f.Fixtures.Workload(workload).Replicas) })
	f.Fixtures.Scale(workload, 0)
	return f.WaitRemediation(v1alpha1.OutcomeHealed)
}

// TestS6ShadowFiles: incident 2. config-a's globalmount is detached, a file
// is written into the bare directory and deploy-single is scaled to 0:
// kubelet cannot unstage (the directory is not empty) and nothing
// references the PV. A Sweep remediation quarantines the file into an
// SmbQuarantine (files=1, remediationRef set). The seeded file name must not
// appear in the SmbQuarantine, in any Event or in the worker's logs.
func TestS6ShadowFiles(t *testing.T) {
	f := framework.New(t, framework.ClearQuarantines())
	g := f.G
	f.WaitBaseline()
	rec := f.RecordSmbMounts()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	w0 := f.Metrics(metrics.ComponentWorker)
	worker := workerPod(f)
	since := time.Now()

	pv := framework.PV("config-a")
	secret := "e2e-private-" + randomHex(8) + ".txt"
	r := injectShadowFiles(f, pv, "deploy-single", secret)
	checkSweep(f, r, pv, v1alpha1.TriggerGlobalmountNotEmpty)
	g.Expect(rec.Stages(v1alpha1.SmbMountName(f.AgentNode, pv), r.Name)).NotTo(gomega.BeEmpty(),
		"STAGE shown on the SmbMount:\n%s", rec.Describe(v1alpha1.SmbMountName(f.AgentNode, pv)))
	q := checkQuarantined(f, r, pv, 1)
	g.Expect(q.Spec.SourcePath).To(gomega.Equal(f.Fixtures.GlobalmountPath(pv)))
	g.Expect(q.OwnerReferences).To(gomega.ContainElement(gomega.HaveField("Kind", "Node")), "owned by the node")

	// Metrics: one quarantine action that moved data; the gauge shows it.
	g.Eventually(func(g gomega.Gomega) {
		w := f.Metrics(metrics.ComponentWorker)
		g.Expect(w.Value(metrics.Quarantines, framework.LabelsOf(metrics.LabelPV, pv))).To(gomega.Equal(1.0),
			"quarantines{pv}:\n%s", w.Series(metrics.Quarantines))
	}).Should(gomega.Succeed())
	w := framework.Delta(w0, f.Metrics(metrics.ComponentWorker))
	g.Expect(w.Get(metrics.HostActionsTotal, metrics.LabelAction, "quarantine", metrics.LabelResult, "succeeded")).To(gomega.Equal(1.0),
		"host_actions{quarantine}:\n%s", w.After.Series(metrics.HostActionsTotal))
	moved := 0
	for _, res := range hostResults(f, r.Name)[v1alpha1.HostActionQuarantine] {
		if res.Destination != "" {
			moved++
			g.Expect(res.Destination).To(gomega.Equal(q.Spec.Path))
			g.Expect(res.Entries).To(gomega.Equal(int32(1)))
		}
	}
	g.Expect(moved).To(gomega.Equal(1), "quarantine results with a destination")
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(cp.Get(metrics.RemediationsStartedTotal, metrics.LabelType, string(v1alpha1.RemediationTypeSweep))).To(gomega.Equal(1.0))
	g.Expect(cp.Value(metrics.WorkloadActionsTotal, nil)).To(gomega.BeZero(), "a sweep never scales")
	g.Expect(f.EventReasons("SmbQuarantine", q.Name, since)).To(gomega.ContainElement("Warning/ShadowDataQuarantined"))

	// Privacy: the seeded name appears nowhere.
	raw, err := json.Marshal(f.Quarantines())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(string(raw)).NotTo(gomega.ContainSubstring(secret), "smbq YAML")
	g.Expect(f.AllEventsText(since)).NotTo(gomega.ContainSubstring(secret), "events")
	logs := f.PodLogs(framework.HealerNamespace, worker.Name, "", since)
	g.Expect(logs).NotTo(gomega.BeEmpty())
	g.Expect(logs).To(gomega.ContainSubstring(q.Spec.Path), "worker logs mention the quarantine folder")
	g.Expect(logs).NotTo(gomega.ContainSubstring(secret), "worker logs")
	// Ground truth: the file is in the quarantine folder, not lost.
	f.Chaos.Sh(f.AgentNode, "test -f "+framework.ShellQuote(filepath.Join(q.Spec.Path, secret)))
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}

// TestS7OrphanPodDir: incident 3. A pod volume directory of a pod UID that
// does not exist holds a file, for a PV no pod on the node uses. A Sweep
// remediation quarantines it.
func TestS7OrphanPodDir(t *testing.T) {
	f := framework.New(t, framework.ClearQuarantines())
	g := f.G
	f.WaitBaseline()
	cp0 := f.Metrics(metrics.ComponentControlPlane)
	w0 := f.Metrics(metrics.ComponentWorker)

	pv := framework.PV("landmine")
	uid := uuid.NewUUID()
	dir := f.Fixtures.PodVolumePath(uid, pv)
	podDir := filepath.Join(framework.KubeletRoot, "pods", string(uid))
	f.Logf("inject: orphan pod dir %s with old.txt", dir)
	t.Cleanup(func() { f.Chaos.Sh(f.AgentNode, "rm -rf "+framework.ShellQuote(podDir)) })
	f.Chaos.WriteFile(f.AgentNode, filepath.Join(dir, "old.txt"), "left behind by a pod that is long gone")

	r := f.WaitRemediation(v1alpha1.OutcomeHealed)
	checkSweep(f, r, pv, v1alpha1.TriggerOrphanPodDirNotEmpty)
	q := checkQuarantined(f, r, pv, 1)
	g.Expect(q.Spec.SourcePath).To(gomega.Equal(dir))
	w := framework.Delta(w0, f.Metrics(metrics.ComponentWorker))
	g.Expect(w.Get(metrics.HostActionsTotal, metrics.LabelAction, "quarantine", metrics.LabelResult, "succeeded")).To(gomega.Equal(1.0),
		"host_actions{quarantine}:\n%s", w.After.Series(metrics.HostActionsTotal))
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(cp.Value(metrics.WorkloadActionsTotal, nil)).To(gomega.BeZero())
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}

// TestS8DeadOrphanGlobalmount: samba-landmine's share is cifs-mounted onto
// the landmine PV's globalmount (no pod uses it) and onto a globalmount
// under a hash no PV maps to; then samba-landmine is scaled to 0. The
// known, dead, unreferenced globalmount is force-unmounted by a Sweep; the
// unknown one is left alone.
func TestS8DeadOrphanGlobalmount(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()
	w0 := f.Metrics(metrics.ComponentWorker)
	cp0 := f.Metrics(metrics.ComponentControlPlane)

	pv := framework.PV("landmine")
	gm := f.Fixtures.GlobalmountPath(pv)
	unknown := filepath.Join(filepath.Dir(filepath.Dir(gm)), randomHex(32), "globalmount")
	unc := fmt.Sprintf("//samba-landmine.%s.svc.cluster.local/landmine", framework.SambaNamespace)

	t.Cleanup(func() { f.Landmine.Scale(1) })
	t.Cleanup(func() {
		f.Chaos.Sh(f.AgentNode, fmt.Sprintf(
			// findmnt reads mountinfo; mountpoint(1) would stat the dead mount.
			"for p in %[1]s %[2]s; do while findmnt -rn --mountpoint \"$p\" >/dev/null; do umount -f -l \"$p\" || break; done; done; rm -rf %[3]s %[4]s",
			framework.ShellQuote(unknown), framework.ShellQuote(gm),
			framework.ShellQuote(filepath.Dir(unknown)), framework.ShellQuote(filepath.Dir(gm))))
	})
	f.Logf("inject: mount %s on %s and on unknown %s", unc, gm, unknown)
	f.Chaos.MountCIFS(f.AgentNode, unc, gm)
	f.Chaos.MountCIFS(f.AgentNode, unc, unknown)
	name := v1alpha1.SmbMountName(f.AgentNode, pv)
	g.Eventually(func(g gomega.Gomega) {
		m := f.SmbMount(f.AgentNode, pv)
		g.Expect(m).NotTo(gomega.BeNil(), "smbm %s", name)
		g.Expect(m.Status.Globalmount).NotTo(gomega.BeNil())
		g.Expect(m.Status.Globalmount.Mounted).To(gomega.BeTrue())
		g.Expect(m.Status.Health).To(gomega.Equal(v1alpha1.HealthHealthy))
		w := f.Metrics(metrics.ComponentWorker)
		g.Expect(w.Value(metrics.UnknownGlobalmounts, nil)).To(gomega.BeNumerically(">=", 1), "unknown_globalmounts")
	}).Should(gomega.Succeed())
	rec := f.RecordSmbMounts()

	f.Logf("inject: scale samba-landmine to 0")
	f.Landmine.Scale(0)
	r := f.WaitRemediation(v1alpha1.OutcomeHealed)
	checkSweep(f, r, pv, v1alpha1.TriggerOrphanGlobalmountBroken)
	g.Expect(rec.SawHealth(name, v1alpha1.HealthBroken, false)).To(gomega.BeTrue(), "smbm Broken:\n%s", rec.Describe(name))
	g.Expect(r.Status.DeadGlobalmounts).To(gomega.ContainElement(gomega.HaveField("Path", gm)))
	unmounted := false
	for _, res := range hostResults(f, r.Name)[v1alpha1.HostActionForceUnmount] {
		unmounted = unmounted || res.Result == v1alpha1.HostActionSucceeded
	}
	g.Expect(unmounted).To(gomega.BeTrue(), "a ForceUnmount succeeded")
	w := framework.Delta(w0, f.Metrics(metrics.ComponentWorker))
	g.Expect(w.Get(metrics.HostActionsTotal, metrics.LabelAction, "force_unmount", metrics.LabelResult, "succeeded")).To(gomega.Equal(1.0),
		"host_actions{force_unmount}:\n%s", w.After.Series(metrics.HostActionsTotal))
	cp := framework.Delta(cp0, f.Metrics(metrics.ComponentControlPlane))
	g.Expect(cp.Value(metrics.WorkloadActionsTotal, nil)).To(gomega.BeZero())

	// Node ground truth: the known globalmount is gone, the unknown one untouched.
	fstype, _ := f.Chaos.MountInfo(f.AgentNode, gm)
	g.Expect(fstype).To(gomega.BeEmpty(), "%s unmounted", gm)
	fstype, _ = f.Chaos.MountInfo(f.AgentNode, unknown)
	g.Expect(fstype).To(gomega.Equal("cifs"), "unknown-hash globalmount left untouched")
	for _, t := range f.HostTasksOf(r.Name) {
		for _, a := range t.Spec.Actions {
			g.Expect(a.Path).NotTo(gomega.HavePrefix(filepath.Dir(unknown)), "no action on the unknown globalmount")
		}
	}
	g.Expect(onlyRemediation(f).Name).To(gomega.Equal(r.Name))
}
