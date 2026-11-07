//go:build envtest

package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/common/expfmt"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/linux"
	"github.com/marcherschel/smb-mount-healer/internal/kubelet"
)

func (h *harness) smbq(folder string) (*v1alpha1.SmbQuarantine, error) {
	var q v1alpha1.SmbQuarantine
	err := k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbQuarantineName(h.node, folder)}, &q)
	return &q, err
}

func (h *harness) eventuallySmbq(folder string, check func(g Gomega, q *v1alpha1.SmbQuarantine)) *v1alpha1.SmbQuarantine {
	h.t.Helper()
	var q *v1alpha1.SmbQuarantine
	h.g.Eventually(func(g Gomega) {
		var err error
		q, err = h.smbq(folder)
		g.Expect(err).NotTo(HaveOccurred())
		check(g, q)
	}).Should(Succeed())
	return q
}

func (h *harness) eventuallyNoSmbq(folder string) {
	h.t.Helper()
	h.g.Eventually(func() bool {
		_, err := h.smbq(folder)
		return apierrors.IsNotFound(err)
	}).Should(BeTrue())
}

func TestSmbQuarantineFollowsDisk(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-cfg"
	h.pv(pv, "")
	gm := h.layout.GlobalmountPath(pv)
	// Incident 2: shadow files in an unmounted globalmount dir.
	h.host.Dirs.SetDir(gm, fake.Dir{Entries: 3, Files: 10, Bytes: 4096, ModTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)})
	// Not a healer folder: ignored, never reported.
	h.host.Quarantine.SetFolder("lost+found", hostfs.QuarantineStats{Entries: 1, Files: 1})
	h.start(h.fakeHost())
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Globalmount).To(Equal(&v1alpha1.GlobalmountStatus{DirExists: true}))
	})

	// (1) The control plane quarantines it: an smbq appears with provenance.
	name := h.node + "-sweeping-0"
	h.task(name, false, v1alpha1.HostAction{ID: "q", Type: v1alpha1.HostActionQuarantine, Path: gm, PV: pv, OnlyIf: v1alpha1.OnlyIfNotMountpoint})
	task := h.eventuallyTask(name, v1alpha1.HostTaskSucceeded)
	dest := task.Status.Results[0].Destination
	folder := filepath.Base(dest)
	g := h.g
	g.Expect(folder).To(Equal(kubelet.QuarantineFolderName(pv, h.clock.Now())))
	q := h.eventuallySmbq(folder, func(g Gomega, q *v1alpha1.SmbQuarantine) {
		g.Expect(q.Status.Files).To(Equal(int64(10)))
	})
	g.Expect(q.Spec.QuarantinedTime.Time).To(BeTemporally("==", h.clock.Now()))
	g.Expect(q.Spec).To(Equal(v1alpha1.SmbQuarantineSpec{
		NodeName: h.node, Path: dest, PVName: pv, Share: "//samba.example/" + pv, SourcePath: gm,
		QuarantinedTime: q.Spec.QuarantinedTime, RemediationRef: "rem-" + h.node,
	}))
	g.Expect(q.Labels).To(Equal(map[string]string{v1alpha1.LabelNode: h.node, v1alpha1.LabelPV: pv}))
	g.Expect(q.OwnerReferences).To(HaveLen(1))
	g.Expect(q.OwnerReferences[0].Kind).To(Equal("Node"))
	g.Expect(q.Status.Entries).To(Equal(int64(3)))
	g.Expect(q.Status.Bytes).To(Equal(int64(4096)))
	g.Expect(q.Status.LastScanTime).NotTo(BeNil())
	g.Expect(q.Status.OldestModTime.Time).To(BeTemporally("==", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)))
	g.Expect(meta.IsStatusConditionTrue(q.Status.Conditions, v1alpha1.ConditionPending)).To(BeTrue())
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.PendingQuarantines).To(Equal(int32(1)))
		g.Expect(meta.IsStatusConditionTrue(sm.Status.Conditions, v1alpha1.ConditionShadowDataPending)).To(BeTrue())
		g.Expect(sm.Status.Globalmount).To(Equal(&v1alpha1.GlobalmountStatus{DirExists: true, Empty: true}))
	})
	g.Expect(metricValue(h.m.Quarantines.WithLabelValues(pv))).To(Equal(1.0))
	g.Expect(metricValue(h.m.QuarantineBytes.WithLabelValues(pv))).To(Equal(4096.0))
	g.Expect(metricValue(h.m.QuarantineOldestTimestamp.WithLabelValues(pv))).To(Equal(float64(h.clock.Now().Unix())))
	g.Eventually(func(g Gomega) {
		g.Expect(eventReasons(g, h, q.UID)).To(ContainElement(EventShadowDataQuarantine))
	}).Should(Succeed())

	// (2) Partial migration: the counts shrink on the next scan.
	h.host.Quarantine.SetFolder(folder, hostfs.QuarantineStats{Entries: 1, Files: 4, Bytes: 1000})
	h.w.Scanner.Trigger()
	h.eventuallySmbq(folder, func(g Gomega, q *v1alpha1.SmbQuarantine) {
		g.Expect(q.Status.Files).To(Equal(int64(4)))
		g.Expect(q.Status.Bytes).To(Equal(int64(1000)))
	})

	// (3) Deleting the smbq does not acknowledge it: recreated while data remains.
	g.Expect(k8s.Delete(h.ctx, q)).To(Succeed())
	h.eventuallySmbq(folder, func(g Gomega, again *v1alpha1.SmbQuarantine) {
		g.Expect(again.UID).NotTo(Equal(q.UID))
		g.Expect(again.Spec.RemediationRef).To(Equal("rem-"+h.node), "provenance survives from the task result")
		g.Expect(again.Status.Files).To(Equal(int64(4)))
	})

	// (4) A worker restart keeps it (no duplicate, provenance from the task).
	h.restart()
	g.Expect(h.w.ScanQuarantine(h.ctx)).To(Succeed())
	var list v1alpha1.SmbQuarantineList
	g.Expect(k8s.List(h.ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: h.node})).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1))

	// (5) The data is moved away: the smbq disappears by itself.
	h.host.Quarantine.RemoveFolder(folder)
	h.w.Scanner.Trigger()
	h.eventuallyNoSmbq(folder)
	g.Eventually(func() int { return seriesCount(h.m.Quarantines) }).Should(BeZero())
	// Only the empty globalmount dir is left: the SmbMount goes with it.
	g.Eventually(func() bool {
		h.probeAll()
		return apierrors.IsNotFound(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, pv)}, &v1alpha1.SmbMount{}))
	}).Should(BeTrue())

	// (6) A folder left by the bash healer is adopted with an empty remediationRef.
	bash := pv + "-20250102T030405Z"
	h.host.Quarantine.SetFolder(bash, hostfs.QuarantineStats{Entries: 2, Files: 2, Bytes: 10})
	// An emptied folder is resolved: no smbq (and nothing is deleted on disk).
	empty := pv + "-20250102T030406Z"
	h.host.Quarantine.SetFolder(empty, hostfs.QuarantineStats{})
	h.w.Scanner.Trigger()
	adopted := h.eventuallySmbq(bash, func(g Gomega, q *v1alpha1.SmbQuarantine) {
		g.Expect(q.Status.Files).To(Equal(int64(2)))
	})
	g.Expect(adopted.Spec.RemediationRef).To(BeEmpty())
	g.Expect(adopted.Spec.SourcePath).To(BeEmpty())
	g.Expect(adopted.Spec.PVName).To(Equal(pv))
	g.Expect(adopted.Spec.QuarantinedTime.Time).To(BeTemporally("==", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)))
	g.Expect(h.w.ScanQuarantine(h.ctx)).To(Succeed())
	_, err := h.smbq(empty)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
	g.Expect(k8s.List(h.ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: h.node})).To(Succeed())
	g.Expect(list.Items).To(HaveLen(1), "lost+found and the empty folder have no smbq")
	g.Expect(metricValue(h.m.Quarantines.WithLabelValues(pv))).To(Equal(1.0))
	g.Expect(metricValue(h.m.QuarantineOldestTimestamp.WithLabelValues(pv))).To(Equal(float64(adopted.Spec.QuarantinedTime.Unix())))
	g.Expect(metricValue(h.m.QuarantineScans.WithLabelValues("success"))).To(BeNumerically(">=", 2))
}

func TestQuarantineScanErrorKeepsObjects(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-err"
	folder := pv + "-20250102T030405Z"
	h.host.Quarantine.SetFolder(folder, hostfs.QuarantineStats{Entries: 1, Files: 1, Bytes: 1})
	h.start(h.fakeHost())
	h.eventuallySmbq(folder, func(Gomega, *v1alpha1.SmbQuarantine) {})
	h.host.Quarantine.SetScanError(os.ErrPermission)
	h.g.Expect(h.w.ScanQuarantine(h.ctx)).To(HaveOccurred())
	_, err := h.smbq(folder)
	h.g.Expect(err).NotTo(HaveOccurred(), "a failed scan must never delete an smbq")
	h.g.Expect(metricValue(h.m.QuarantineScans.WithLabelValues("error"))).To(Equal(1.0))
	h.g.Expect(metricValue(h.m.Quarantines.WithLabelValues(pv))).To(Equal(1.0))
}

// TestNoQuarantinedNameLeaks runs a quarantine on the real hostfs/linux
// implementation (temp dirs, no privileges) and asserts that the names of
// the moved files appear in no CR, Event, metric or log line.
func TestNoQuarantinedNameLeaks(t *testing.T) {
	h := newHarness(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "kubelet")
	qdir := filepath.Join(root, "smb-mount-healer-quarantine")
	h.cfg.Worker.KubeletRoot, h.cfg.Worker.QuarantineDir = root, qdir
	h.layout.Root = root
	mountinfo := filepath.Join(tmp, "mountinfo") // nothing is mounted
	h.g.Expect(os.WriteFile(mountinfo, nil, 0o600)).To(Succeed())

	pv := h.node + "-secret"
	h.pv(pv, "")
	gm := h.layout.GlobalmountPath(pv)
	const canary = "CANARY-tax-return-of-alice"
	h.g.Expect(os.MkdirAll(filepath.Join(gm, canary+"-dir"), 0o755)).To(Succeed())
	h.g.Expect(os.WriteFile(filepath.Join(gm, canary+".pdf"), []byte("secret"), 0o600)).To(Succeed())
	h.g.Expect(os.WriteFile(filepath.Join(gm, canary+"-dir", canary+"-2.txt"), []byte("x"), 0o600)).To(Succeed())

	mounts := linux.MountTable{Prefix: root, Path: mountinfo}
	h.start(Host{
		Mounts:     mounts,
		Dirs:       linux.DirInspector{Layout: h.layout, Mounts: mounts},
		Prober:     h.host.Prober,
		Mutator:    &linux.Mutator{Root: root, Mounts: mounts},
		Quarantine: linux.QuarantineInspector{Dir: qdir, MaxEntries: 1000, TimeBudget: 10 * time.Second, Mounts: linux.MountTable{Prefix: qdir, Path: mountinfo}},
	})
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Globalmount).To(Equal(&v1alpha1.GlobalmountStatus{DirExists: true}))
	})
	name := h.node + "-quarantining-0"
	h.task(name, false, v1alpha1.HostAction{ID: "q", Type: v1alpha1.HostActionQuarantine, Path: gm, PV: pv, OnlyIf: v1alpha1.OnlyIfNotMountpoint})
	task := h.eventuallyTask(name, v1alpha1.HostTaskSucceeded)
	dest := task.Status.Results[0].Destination
	g := h.g
	g.Expect(dest).To(HavePrefix(qdir + "/"))
	q := h.eventuallySmbq(filepath.Base(dest), func(g Gomega, q *v1alpha1.SmbQuarantine) {
		g.Expect(q.Status.Files).To(Equal(int64(2)))
		g.Expect(q.Status.Entries).To(Equal(int64(2)))
	})
	// The data was moved, not deleted.
	g.Expect(filepath.Join(dest, canary+"-dir", canary+"-2.txt")).To(BeAnExistingFile())
	h.probeAll()
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.PendingQuarantines).To(Equal(int32(1)))
	})
	g.Eventually(func(g Gomega) {
		g.Expect(eventReasons(g, h, q.UID)).To(ContainElement(EventShadowDataQuarantine))
	}).Should(Succeed())

	var dump strings.Builder
	add := func(v any) {
		b, err := json.Marshal(v)
		g.Expect(err).NotTo(HaveOccurred())
		dump.Write(b)
	}
	var sms v1alpha1.SmbMountList
	var sqs v1alpha1.SmbQuarantineList
	var sts v1alpha1.SmbHostTaskList
	var evs eventsv1.EventList
	for _, l := range []client.ObjectList{&sms, &sqs, &sts, &evs} {
		g.Expect(k8s.List(h.ctx, l)).To(Succeed())
		add(l)
	}
	mfs, err := h.reg.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	for _, mf := range mfs {
		_, err := expfmt.MetricFamilyToText(&dump, mf)
		g.Expect(err).NotTo(HaveOccurred())
	}
	dump.WriteString(h.logs.all())
	out := dump.String()
	g.Expect(out).To(ContainSubstring(filepath.Base(dest)), "sanity: the dump covers the quarantine")
	g.Expect(h.logs.all()).To(ContainSubstring("quarantined local shadow data"), "sanity: logs were captured")
	g.Expect(strings.Contains(out, "CANARY")).To(BeFalse(), "a quarantined entry name leaked")
	g.Expect(strings.Contains(out, "tax-return")).To(BeFalse(), "a quarantined entry name leaked")
}
