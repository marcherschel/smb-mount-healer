//go:build envtest

package worker

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
)

// task creates an SmbHostTask for h.node, labelled like the control plane does.
func (h *harness) task(name string, dryRun bool, actions ...v1alpha1.HostAction) *v1alpha1.SmbHostTask {
	h.t.Helper()
	t := &v1alpha1.SmbHostTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{v1alpha1.LabelNode: h.node, v1alpha1.LabelRemediation: "rem-" + h.node},
		},
		Spec: v1alpha1.SmbHostTaskSpec{NodeName: h.node, DryRun: dryRun, Actions: actions},
	}
	h.g.Expect(k8s.Create(h.ctx, t)).To(Succeed())
	return t
}

func (h *harness) eventuallyTask(name string, phase v1alpha1.HostTaskPhase) *v1alpha1.SmbHostTask {
	h.t.Helper()
	var t v1alpha1.SmbHostTask
	h.g.Eventually(func(g Gomega) {
		g.Expect(k8s.Get(h.ctx, client.ObjectKey{Name: name}, &t)).To(Succeed())
		g.Expect(t.Status.Phase).To(Equal(phase))
		g.Expect(t.Status.ObservedGeneration).To(Equal(t.Generation))
	}).Should(Succeed())
	return &t
}

func (h *harness) hostActions(action, result string, dryRun bool) float64 {
	dr := "false"
	if dryRun {
		dr = "true"
	}
	return metricValue(h.m.HostActions.WithLabelValues(action, result, dr))
}

func resultsByID(t *v1alpha1.SmbHostTask) map[string]v1alpha1.HostActionResult {
	out := map[string]v1alpha1.HostActionResult{}
	for _, r := range t.Status.Results {
		out[r.ID] = r
	}
	return out
}

func TestHostTaskIdempotentAcrossRestart(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-tx"
	h.pv(pv, "")
	gm := h.layout.GlobalmountPath(pv)
	pod := h.layout.PodVolumeMountPath("dead-uid", pv)
	h.host.Mounts.Mount(pod, "xfs", "/dev/vda6", "")
	h.host.Dirs.SetDir(gm, fake.Dir{Entries: 2, Files: 3, Bytes: 42})

	name := h.node + "-quarantining-0"
	task := h.task(name, false,
		v1alpha1.HostAction{ID: "a1", Type: v1alpha1.HostActionForceUnmount, Path: pod, PV: pv, OnlyIf: v1alpha1.OnlyIfNotHealthyMount},
		v1alpha1.HostAction{ID: "a2", Type: v1alpha1.HostActionQuarantine, Path: gm, PV: pv, OnlyIf: v1alpha1.OnlyIfNotMountpoint},
		v1alpha1.HostAction{ID: "a3", Type: v1alpha1.HostActionProbeNow, Path: gm},
	)
	// A previous worker finished a1 and died before a2.
	done := metav1.NewTime(time.Date(2026, 10, 1, 1, 2, 3, 0, time.UTC))
	task.Status = v1alpha1.SmbHostTaskStatus{Phase: v1alpha1.HostTaskRunning, ObservedGeneration: task.Generation,
		Results: []v1alpha1.HostActionResult{{ID: "a1", Result: v1alpha1.HostActionSucceeded, Time: done}}}
	h.g.Expect(k8s.Status().Update(h.ctx, task)).To(Succeed())

	h.start(h.fakeHost())
	got := h.eventuallyTask(name, v1alpha1.HostTaskSucceeded)
	g := h.g
	res := resultsByID(got)
	g.Expect(res).To(HaveLen(3))
	g.Expect(res["a1"].Time.Time).To(BeTemporally("==", done.Time), "a1 must not be re-run")
	g.Expect(res["a2"].Result).To(Equal(v1alpha1.HostActionSucceeded))
	g.Expect(res["a2"].Entries).To(Equal(int32(2)))
	g.Expect(res["a2"].Destination).To(HavePrefix(testQuarantine + "/" + pv + "-"))
	g.Expect(res["a3"].Result).To(Equal(v1alpha1.HostActionSucceeded))
	calls := h.host.Mutator.Calls()
	g.Expect(calls).To(HaveLen(1))
	g.Expect(calls[0].Op).To(Equal("Quarantine"))
	g.Expect(h.host.Mounts.IsMountpoint(pod)).To(BeTrue(), "a1 was already done; the worker must not redo it")
	g.Expect(h.hostActions(ActionLabelQuarantine, ResultLabelSucceeded, false)).To(Equal(1.0))
	g.Expect(h.hostActions(ActionLabelProbeNow, ResultLabelSucceeded, false)).To(Equal(1.0))
	g.Expect(h.hostActions(ActionLabelForceUnmount, ResultLabelSucceeded, false)).To(BeZero())
	// The ProbeNow made the task wait for a published round that probed gm.
	g.Expect(h.w.Publisher.publishedOnce()).To(BeTrue())

	// A restarted worker sees a complete task and does nothing.
	h.restart()
	h.probeAll()
	g.Consistently(func() int { return len(h.host.Mutator.Calls()) }, 500*time.Millisecond).Should(Equal(1))
	g.Expect(seriesCount(h.m.HostActions)).To(BeZero())
	var again v1alpha1.SmbHostTask
	g.Expect(k8s.Get(h.ctx, client.ObjectKey{Name: name}, &again)).To(Succeed())
	g.Expect(again.Status.Results).To(Equal(got.Status.Results))
	g.Expect(again.ResourceVersion).To(Equal(got.ResourceVersion), "no status write for a finished task")
}

func TestDryRunHostTaskPerformsNoMutation(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-dry"
	h.pv(pv, "")
	gm := h.layout.GlobalmountPath(pv)
	pod := h.layout.PodVolumeMountPath("dead-uid", pv)
	h.host.Mounts.Mount(pod, "xfs", "/dev/vda6", "")
	h.host.Dirs.SetDir(gm, fake.Dir{Entries: 1, Files: 1, Bytes: 7})
	h.start(h.fakeHost())

	name := h.node + "-sweeping-0"
	h.task(name, true,
		v1alpha1.HostAction{ID: "u", Type: v1alpha1.HostActionForceUnmount, Path: pod, OnlyIf: v1alpha1.OnlyIfNotHealthyMount},
		v1alpha1.HostAction{ID: "q", Type: v1alpha1.HostActionQuarantine, Path: gm, PV: pv, OnlyIf: v1alpha1.OnlyIfNotMountpoint},
		v1alpha1.HostAction{ID: "bad", Type: v1alpha1.HostActionForceUnmount, Path: "/etc"},
	)
	got := h.eventuallyTask(name, v1alpha1.HostTaskFailed)
	g := h.g
	res := resultsByID(got)
	g.Expect(res["u"].Result).To(Equal(v1alpha1.HostActionDryRun))
	g.Expect(res["q"].Result).To(Equal(v1alpha1.HostActionDryRun))
	g.Expect(res["q"].Destination).To(BeEmpty())
	g.Expect(res["bad"].Result).To(Equal(v1alpha1.HostActionFailed))
	g.Expect(h.host.Mutator.Calls()).To(BeEmpty())
	g.Expect(h.host.Mounts.IsMountpoint(pod)).To(BeTrue())
	folders, err := h.host.Quarantine.ListFolders(h.ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(folders).To(BeEmpty())
	g.Expect(h.hostActions(ActionLabelForceUnmount, ResultLabelDryRun, true)).To(Equal(1.0))
	g.Expect(h.hostActions(ActionLabelQuarantine, ResultLabelDryRun, true)).To(Equal(1.0))
	g.Expect(h.hostActions(ActionLabelForceUnmount, ResultLabelFailed, true)).To(Equal(1.0))
	var list v1alpha1.SmbQuarantineList
	g.Expect(k8s.List(h.ctx, &list, client.MatchingLabels{v1alpha1.LabelNode: h.node})).To(Succeed())
	g.Expect(list.Items).To(BeEmpty())
}

func TestHostTaskOfOtherNodeIgnored(t *testing.T) {
	h := newHarness(t)
	h.start(h.fakeHost())
	other := &v1alpha1.SmbHostTask{
		// Mislabelled: carries our node label but targets another node.
		ObjectMeta: metav1.ObjectMeta{Name: h.node + "-other", Labels: map[string]string{v1alpha1.LabelNode: h.node}},
		Spec: v1alpha1.SmbHostTaskSpec{NodeName: "someone-else", Actions: []v1alpha1.HostAction{
			{ID: "p", Type: v1alpha1.HostActionProbeNow, Path: testRoot + "/x"},
		}},
	}
	h.g.Expect(k8s.Create(h.ctx, other)).To(Succeed())
	h.g.Consistently(func(g Gomega) {
		var t v1alpha1.SmbHostTask
		g.Expect(k8s.Get(h.ctx, client.ObjectKey{Name: other.Name}, &t)).To(Succeed())
		g.Expect(t.Status.Phase).To(BeEmpty())
	}, 500*time.Millisecond).Should(Succeed())
}
