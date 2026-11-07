//go:build e2e

package e2e

import (
	"strconv"
	"strings"
	"testing"

	"github.com/onsi/gomega"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
	"github.com/marcherschel/smb-mount-healer/test/e2e/framework"
)

// TestS1Baseline: with no fault injected, every SmbMount is Healthy, the
// globalmounts are cifs with `soft` among the super options (the fixtures never
// set it; the cifs default must provide it), heartbeats reach the samba shares,
// and the control plane starts no remediation.
func TestS1Baseline(t *testing.T) {
	f := framework.New(t)
	g := f.G
	f.WaitBaseline()

	cpBefore := f.Metrics(metrics.ComponentControlPlane)
	wBefore := f.Metrics(metrics.ComponentWorker)
	g.Expect(wBefore.Value(metrics.BuildInfo, framework.LabelsOf(metrics.LabelMode, metrics.ComponentWorker))).
		To(gomega.Equal(1.0), "worker build_info:\n%s", wBefore.Series(metrics.BuildInfo))
	g.Expect(cpBefore.Value(metrics.BuildInfo, framework.LabelsOf(metrics.LabelMode, metrics.ComponentControlPlane))).
		To(gomega.Equal(1.0), "controlplane build_info:\n%s", cpBefore.Series(metrics.BuildInfo))

	for _, pv := range framework.StagedPVs() {
		m := f.SmbMount(f.AgentNode, pv)
		g.Expect(m).NotTo(gomega.BeNil(), "smbm for %s", pv)
		g.Expect(m.Spec.NodeName).To(gomega.Equal(f.AgentNode))
		g.Expect(m.Spec.PVName).To(gomega.Equal(pv))
		g.Expect(m.Spec.VolumeHandle).To(gomega.Equal(pv), "tofu shape: name == volumeHandle")
		g.Expect(m.Spec.GlobalmountPath).To(gomega.Equal(f.Fixtures.GlobalmountPath(pv)))
		g.Expect(m.Status.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "smbm %s", m.Name)

		gm := m.Status.Globalmount
		g.Expect(gm).NotTo(gomega.BeNil(), "smbm %s globalmount", m.Name)
		g.Expect(gm.Mounted).To(gomega.BeTrue(), "smbm %s globalmount mounted", m.Name)
		g.Expect(gm.FSType).To(gomega.Equal("cifs"), "smbm %s globalmount fstype", m.Name)
		g.Expect(strings.Split(gm.SuperOptions, ",")).To(gomega.ContainElement("soft"),
			"smbm %s superOptions %q", m.Name, gm.SuperOptions)

		g.Expect(m.Status.PodMounts).NotTo(gomega.BeEmpty(), "smbm %s podMounts", m.Name)
		for _, pm := range m.Status.PodMounts {
			g.Expect(pm.Mounted).To(gomega.BeTrue(), "smbm %s pod %s/%s", m.Name, pm.Namespace, pm.Pod)
			g.Expect(pm.FSType).To(gomega.Equal("cifs"), "smbm %s pod %s/%s fstype", m.Name, pm.Namespace, pm.Pod)
			g.Expect(pm.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "smbm %s pod %s/%s", m.Name, pm.Namespace, pm.Pod)
		}

		// Ground truth from the node, independent of the worker.
		fstype, superOpts := f.Chaos.MountInfo(f.AgentNode, f.Fixtures.GlobalmountPath(pv))
		g.Expect(fstype).To(gomega.Equal("cifs"), "findmnt %s", pv)
		g.Expect(strings.Split(superOpts, ",")).To(gomega.ContainElement("soft"), "findmnt %s: %q", pv, superOpts)
	}

	// Heartbeats: every running fixture pod's file shows up on the server
	// side and keeps advancing.
	for _, w := range framework.Workloads {
		if w.Replicas == 0 || len(w.Volumes) == 0 {
			continue
		}
		first, err := f.Fixtures.Heartbeat(w.Name)
		g.Expect(err).NotTo(gomega.HaveOccurred(), "heartbeat %s", w.Name)
		g.Eventually(func(g gomega.Gomega) {
			now, err := f.Fixtures.Heartbeat(w.Name)
			g.Expect(err).NotTo(gomega.HaveOccurred())
			for k, v := range now {
				g.Expect(epoch(v)).To(gomega.BeNumerically(">", epoch(first[k])), "heartbeat %s advances", k)
			}
		}).Should(gomega.Succeed(), "heartbeats of %s", w.Name)
	}

	// Steady state: nothing gets remediated.
	g.Consistently(func() []string {
		var names []string
		for _, r := range f.NewRemediations() {
			names = append(names, r.Name)
		}
		return names
	}).Should(gomega.BeEmpty(), "no remediation at baseline")
	cpAfter := f.Metrics(metrics.ComponentControlPlane)
	d := framework.Delta(cpBefore, cpAfter)
	g.Expect(d.Value(metrics.RemediationsStartedTotal, nil)).To(gomega.BeZero(),
		"remediations_started:\n%s", cpAfter.Series(metrics.RemediationsStartedTotal))

	// Every SmbMount is still Healthy and nothing was quarantined.
	for _, m := range f.SmbMounts() {
		g.Expect(m.Status.Health).To(gomega.Equal(v1alpha1.HealthHealthy), "smbm %s", m.Name)
	}
	g.Expect(f.Quarantines()).To(gomega.BeEmpty())
}

func epoch(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
