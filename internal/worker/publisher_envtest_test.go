//go:build envtest

package worker

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs"
	"github.com/marcherschel/smb-mount-healer/internal/hostfs/fake"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

func (h *harness) transitions(kind v1alpha1.MountKind, from, to v1alpha1.Health) float64 {
	return metricValue(h.m.HealthTransitions.WithLabelValues(string(kind), string(from), string(to)))
}

func (h *harness) writes() float64 {
	return metricValue(h.m.SmbMountStatusWrites.WithLabelValues(metrics.ResultSuccess))
}

func TestSmbMountCreatedAndHealthTransitionsPublished(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-config"
	h.pv(pv, "config")
	pod := h.pod("app-0", "config")
	gm := h.layout.GlobalmountPath(pv)
	pp := h.podPath(pod.UID, pv)
	h.host.Mounts.Mount(gm, "cifs", "//samba.example/"+pv, "rw,soft")
	h.host.Mounts.Mount(pp, "cifs", "//samba.example/"+pv, "rw,soft")
	h.start(h.fakeHost())

	sm := h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
		g.Expect(sm.Status.PodMounts).To(HaveLen(1))
	})
	g := h.g
	g.Expect(sm.Spec).To(Equal(v1alpha1.SmbMountSpec{NodeName: h.node, PVName: pv, Share: "//samba.example/" + pv,
		VolumeHandle: pv, GlobalmountPath: gm}))
	g.Expect(sm.Labels).To(HaveKeyWithValue(v1alpha1.LabelNode, h.node))
	g.Expect(sm.Labels).To(HaveKeyWithValue(v1alpha1.LabelPV, pv))
	g.Expect(sm.OwnerReferences).To(HaveLen(1))
	g.Expect(sm.OwnerReferences[0].Kind).To(Equal("Node"))
	g.Expect(*sm.OwnerReferences[0].BlockOwnerDeletion).To(BeFalse())
	g.Expect(sm.Status.Globalmount).To(Equal(&v1alpha1.GlobalmountStatus{Mounted: true, FSType: "cifs", SuperOptions: "rw,soft", DirExists: true}))
	g.Expect(sm.Status.PodMounts[0]).To(Equal(v1alpha1.PodMountStatus{Namespace: h.ns, Pod: "app-0", UID: pod.UID,
		Mounted: true, FSType: "cifs", Health: v1alpha1.HealthHealthy}))
	g.Expect(sm.Status.Debounce).To(BeNil())
	g.Expect(meta.IsStatusConditionTrue(sm.Status.Conditions, v1alpha1.ConditionHealthy)).To(BeTrue())
	g.Expect(meta.IsStatusConditionFalse(sm.Status.Conditions, v1alpha1.ConditionShadowDataPending)).To(BeTrue())
	g.Expect(h.transitions(v1alpha1.MountKindGlobalmount, v1alpha1.HealthUnknown, v1alpha1.HealthHealthy)).To(Equal(1.0))

	// A blip: one stat_error, then fine again -> Suspect -> Healthy (false alarm).
	h.host.Prober.Script(gm, hostfs.ProbeStatError)
	h.probeAll()
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthSuspect))
		g.Expect(sm.Status.Reason).To(Equal(v1alpha1.ReasonStatError))
		g.Expect(sm.Status.Debounce).NotTo(BeNil())
		g.Expect(sm.Status.Debounce.ConsecutiveFailures).To(Equal(int32(1)))
	})
	h.probeAll()
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
		g.Expect(sm.Status.Reason).To(BeEmpty())
		g.Expect(sm.Status.Debounce).To(BeNil())
	})
	g.Expect(metricValue(h.m.FalseAlarms.WithLabelValues("globalmount", "stat_error"))).To(Equal(1.0))

	// A real failure: confirmed after RECHECK_ATTEMPTS.
	h.host.Prober.SetResult(gm, hostfs.ProbeStatError, "EIO")
	for range 3 {
		h.probeAll()
	}
	sm = h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthBroken))
		g.Expect(sm.Status.Reason).To(Equal(v1alpha1.ReasonStatError))
		g.Expect(sm.Status.Debounce.ConsecutiveFailures).To(Equal(int32(3)))
	})
	g.Expect(sm.Status.PodMounts[0].Health).To(Equal(v1alpha1.HealthHealthy))
	c := meta.FindStatusCondition(sm.Status.Conditions, v1alpha1.ConditionHealthy)
	g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c.Reason).To(Equal("stat_error"))
	g.Expect(h.transitions(v1alpha1.MountKindGlobalmount, v1alpha1.HealthSuspect, v1alpha1.HealthBroken)).To(Equal(1.0))
	g.Expect(metricValue(h.m.MountStatus.WithLabelValues("globalmount", pv, "//samba.example/"+pv, "", "", "Broken"))).To(Equal(1.0))
	g.Expect(metricValue(h.m.MountStatus.WithLabelValues("globalmount", pv, "//samba.example/"+pv, "", "", "Healthy"))).To(Equal(0.0))
	g.Expect(metricValue(h.m.MountChecks.WithLabelValues("globalmount", "stat_error"))).To(Equal(4.0))

	// The pod mount turns into an xfs bind mount (incident 1 shape).
	h.host.Mounts.Mount(pp, "xfs", "/dev/vda6", "")
	for range 3 {
		h.probeAll()
	}
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.PodMounts[0].Health).To(Equal(v1alpha1.HealthBroken))
		g.Expect(sm.Status.PodMounts[0].Reason).To(Equal(v1alpha1.ReasonWrongFSType))
		g.Expect(sm.Status.PodMounts[0].FSType).To(Equal("xfs"))
	})

	// Recovery.
	h.host.Prober.SetResult(gm, hostfs.ProbeOK, "")
	h.host.Mounts.Unmount(pp)
	h.probeAll()
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
		g.Expect(sm.Status.PodMounts[0].Health).To(Equal(v1alpha1.HealthHealthy))
	})
	g.Expect(h.transitions(v1alpha1.MountKindGlobalmount, v1alpha1.HealthBroken, v1alpha1.HealthHealthy)).To(Equal(1.0))

	// Events for Broken and recovery.
	g.Eventually(func(g Gomega) {
		reasons := eventReasons(g, h, sm.UID)
		g.Expect(reasons).To(ContainElements(EventMountBroken, EventMountRecovered))
	}).Should(Succeed())
}

func TestRestartRestoresDebounceAndDoesNotRetrigger(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-dl"
	h.pv(pv, "")
	gm := h.layout.GlobalmountPath(pv)
	h.host.Mounts.Mount(gm, "cifs", "//samba.example/"+pv, "soft")
	h.host.Prober.SetResult(gm, hostfs.ProbeStatTimeout, "timeout")
	h.start(h.fakeHost()) // first round: failure 1
	h.probeAll()          // failure 2
	h.probeAll()          // failure 3, but slow and still inside SLOW_CONFIRM_WINDOW
	before := h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthSuspect))
		g.Expect(sm.Status.Debounce.ConsecutiveFailures).To(Equal(int32(3)))
	})
	first := before.Status.Debounce.FirstFailureTime.Time

	// Restart after the slow window: the restored streak confirms right away.
	h.stop()
	h.clock.Step(h.cfg.Worker.SlowConfirmWindow)
	h.start(h.fakeHost())
	after := h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthBroken))
	})
	g := h.g
	g.Expect(after.Status.Debounce.ConsecutiveFailures).To(Equal(int32(4)))
	g.Expect(after.Status.Debounce.FirstFailureTime.Time).To(BeTemporally("==", first))
	g.Expect(h.transitions(v1alpha1.MountKindGlobalmount, v1alpha1.HealthSuspect, v1alpha1.HealthBroken)).To(Equal(1.0))
	g.Expect(h.transitions(v1alpha1.MountKindGlobalmount, v1alpha1.HealthUnknown, v1alpha1.HealthSuspect)).To(Equal(0.0))

	// Another restart while still broken: no transition at all, stays Broken.
	h.restart()
	h.probeAll()
	sm := h.smbMount(pv)
	g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthBroken))
	g.Expect(seriesCount(h.m.HealthTransitions)).To(BeZero())
	g.Expect(sm.Status.Debounce.FirstFailureTime.Time).To(BeTemporally("==", first))
}

func TestStatusHeartbeatRateLimitsWrites(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-hb"
	h.pv(pv, "")
	h.host.Mounts.Mount(h.layout.GlobalmountPath(pv), "cifs", "//samba.example/"+pv, "soft")
	h.start(h.fakeHost())
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
	})
	g := h.g
	g.Eventually(h.writes).Should(Equal(1.0))
	lpt := h.smbMount(pv).Status.LastProbeTime.Time

	for range 5 {
		h.probeAll()
	}
	h.clock.Step(time.Minute)
	h.probeAll()
	g.Expect(h.w.Publish(h.ctx)).To(Succeed())
	g.Consistently(h.writes, 300*time.Millisecond).Should(Equal(1.0))
	g.Expect(h.smbMount(pv).Status.LastProbeTime.Time).To(BeTemporally("==", lpt))

	// Heartbeat due: lastProbeTime is refreshed exactly once.
	h.clock.Step(h.cfg.Worker.StatusHeartbeat)
	h.probeAll()
	g.Eventually(h.writes).Should(Equal(2.0))
	g.Expect(h.smbMount(pv).Status.LastProbeTime.Time).To(BeTemporally("==", h.clock.Now()))
	h.probeAll()
	g.Consistently(h.writes, 300*time.Millisecond).Should(Equal(2.0))

	// A restart with unchanged state does not write either.
	h.restart()
	h.probeAll()
	g.Consistently(h.writes, 300*time.Millisecond).Should(BeZero())

	// Any content change is written immediately.
	h.host.Prober.Script(h.layout.GlobalmountPath(pv), hostfs.ProbeStatError)
	h.probeAll()
	g.Eventually(h.writes).Should(Equal(1.0))
}

func TestCleanupDeletesSmbMountWithoutLocalTraces(t *testing.T) {
	h := newHarness(t)
	gone, active := h.node+"-gone", h.node+"-active"
	for _, pv := range []string{gone, active} {
		h.pv(pv, "")
		h.host.Mounts.Mount(h.layout.GlobalmountPath(pv), "cifs", "//samba.example/"+pv, "soft")
	}
	h.start(h.fakeHost())
	for _, pv := range []string{gone, active} {
		h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
			g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
		})
	}
	// The control plane marks a remediation active on one of them.
	applyControlPlane(h, v1alpha1.SmbMountName(h.node, active), true)
	h.g.Eventually(func(g Gomega) {
		var sm v1alpha1.SmbMount
		g.Expect(h.w.c.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, active)}, &sm)).To(Succeed())
		g.Expect(sm.Status.Remediation).NotTo(BeNil())
	}).Should(Succeed())

	for _, pv := range []string{gone, active} {
		h.host.Mounts.Unmount(h.layout.GlobalmountPath(pv))
	}
	h.probeAll()
	h.g.Eventually(func() bool {
		return apierrors.IsNotFound(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, gone)}, &v1alpha1.SmbMount{}))
	}).Should(BeTrue())
	// Kept while remediating, with the "gone" globalmount state published.
	h.eventuallySmbMount(active, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Globalmount).To(Equal(&v1alpha1.GlobalmountStatus{}))
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthUnknown))
		g.Expect(sm.Status.Remediation.Active).To(BeTrue())
	})
	// Remediation over: deleted on the next round.
	applyControlPlane(h, v1alpha1.SmbMountName(h.node, active), false)
	h.g.Eventually(func() bool {
		h.probeAll()
		return apierrors.IsNotFound(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, active)}, &v1alpha1.SmbMount{}))
	}).Should(BeTrue())
}

func TestIdleGlobalmountDirGetsNoSmbMount(t *testing.T) {
	h := newHarness(t)
	idle, shadow, live := h.node+"-idle", h.node+"-shadow", h.node+"-live"
	for _, pv := range []string{idle, shadow, live} {
		h.pv(pv, "")
	}
	// Leftover of an earlier unstage: empty, unmounted, no pod on the node.
	h.host.Dirs.SetDir(h.layout.GlobalmountPath(idle), fake.Dir{})
	h.host.Dirs.SetDir(h.layout.GlobalmountPath(shadow), fake.Dir{Entries: 1, Files: 1, Bytes: 7})
	h.host.Mounts.Mount(h.layout.GlobalmountPath(live), "cifs", "//samba.example/"+live, "soft")
	h.start(h.fakeHost())
	h.eventuallySmbMount(live, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
	})
	h.eventuallySmbMount(shadow, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Globalmount.Empty).To(BeFalse())
	})
	h.g.Consistently(func() bool {
		return apierrors.IsNotFound(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, idle)}, &v1alpha1.SmbMount{}))
	}, 2*time.Second).Should(BeTrue())

	// Shadow data moved away: kept while a remediation is active on it.
	applyControlPlane(h, v1alpha1.SmbMountName(h.node, shadow), true)
	h.eventuallySmbMount(shadow, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Remediation).NotTo(BeNil())
	})
	// The publisher decides from the worker's informer cache, which can lag
	// behind the API server: emptying the directory before the cache has the
	// active remediation would make the SmbMount look idle and get it deleted.
	h.g.Eventually(func(g Gomega) {
		var sm v1alpha1.SmbMount
		g.Expect(h.w.c.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, shadow)}, &sm)).To(Succeed())
		g.Expect(remediating(&sm)).To(BeTrue())
	}).Should(Succeed())
	h.host.Dirs.SetDir(h.layout.GlobalmountPath(shadow), fake.Dir{})
	h.probeAll()
	h.eventuallySmbMount(shadow, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Globalmount).To(Equal(&v1alpha1.GlobalmountStatus{DirExists: true, Empty: true}))
	})
	// Remediation over: the idle leftover is deleted.
	applyControlPlane(h, v1alpha1.SmbMountName(h.node, shadow), false)
	h.g.Eventually(func() bool {
		h.probeAll()
		return apierrors.IsNotFound(k8s.Get(h.ctx, client.ObjectKey{Name: v1alpha1.SmbMountName(h.node, shadow)}, &v1alpha1.SmbMount{}))
	}).Should(BeTrue())
}

// applyControlPlane applies the control plane's SmbMount status fields.
func applyControlPlane(h *harness, name string, active bool) {
	h.t.Helper()
	now := metav1.NewTime(time.Now().Truncate(time.Second))
	st := ac.SmbMountStatus().
		WithRemediation(ac.ActiveRemediation().WithActive(active).WithName("rem-1").WithStage(v1alpha1.StageDraining).WithStageEnteredTime(now)).
		WithLastRemediation(ac.LastRemediation().WithName("rem-0").WithOutcome(v1alpha1.OutcomeHealed)).
		WithRemediationsLastHour(2).
		WithConditions(metav1ac.Condition().WithType(v1alpha1.ConditionRemediating).WithStatus(metav1.ConditionTrue).
			WithReason("Draining").WithMessage("remediation rem-1 is draining").WithLastTransitionTime(now))
	h.g.Expect(k8s.Status().Apply(h.ctx, ac.SmbMount(name).WithStatus(st),
		client.FieldOwner(v1alpha1.FieldManagerControlPlane), client.ForceOwnership)).To(Succeed())
}

func TestWorkerAppliesNeverTouchControlPlaneFields(t *testing.T) {
	h := newHarness(t)
	pv := h.node + "-cp"
	h.pv(pv, "")
	gm := h.layout.GlobalmountPath(pv)
	h.host.Mounts.Mount(gm, "cifs", "//samba.example/"+pv, "soft")
	h.start(h.fakeHost())
	name := v1alpha1.SmbMountName(h.node, pv)
	h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthHealthy))
	})
	applyControlPlane(h, name, true)

	// Several worker writes (status changes, heartbeat).
	h.host.Prober.SetResult(gm, hostfs.ProbeWrongFSType, "")
	h.host.Mounts.Mount(gm, "xfs", "/dev/vda6", "")
	for range 3 {
		h.probeAll()
	}
	h.clock.Step(h.cfg.Worker.StatusHeartbeat)
	h.probeAll()
	sm := h.eventuallySmbMount(pv, func(g Gomega, sm *v1alpha1.SmbMount) {
		g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthBroken))
		g.Expect(sm.Status.Reason).To(Equal(v1alpha1.ReasonWrongFSType))
	})
	g := h.g
	g.Expect(sm.Status.Remediation).To(Equal(&v1alpha1.ActiveRemediation{Active: true, Name: "rem-1",
		Stage: v1alpha1.StageDraining, StageEnteredTime: sm.Status.Remediation.StageEnteredTime}))
	g.Expect(sm.Status.LastRemediation).To(Equal(&v1alpha1.LastRemediation{Name: "rem-0", Outcome: v1alpha1.OutcomeHealed}))
	g.Expect(sm.Status.RemediationsLastHour).To(Equal(int32(2)))
	g.Expect(meta.IsStatusConditionTrue(sm.Status.Conditions, v1alpha1.ConditionRemediating)).To(BeTrue())
	g.Expect(meta.IsStatusConditionFalse(sm.Status.Conditions, v1alpha1.ConditionHealthy)).To(BeTrue())

	// The worker's field set never mentions control-plane fields.
	for _, mf := range sm.ManagedFields {
		if mf.Manager != v1alpha1.FieldManagerWorker || mf.Subresource != "status" {
			continue
		}
		raw := mf.FieldsV1.GetRawString()
		for _, f := range []string{"f:remediation", "f:lastRemediation", "f:remediationsLastHour", "Remediating"} {
			g.Expect(raw).NotTo(ContainSubstring(f))
		}
		g.Expect(raw).To(ContainSubstring("f:health"))
	}
	// And the control plane can still release its fields independently.
	g.Expect(k8s.Status().Apply(h.ctx, ac.SmbMount(name).WithStatus(ac.SmbMountStatus()),
		client.FieldOwner(v1alpha1.FieldManagerControlPlane), client.ForceOwnership)).To(Succeed())
	sm = h.smbMount(pv)
	g.Expect(sm.Status.Remediation).To(BeNil())
	g.Expect(sm.Status.Health).To(Equal(v1alpha1.HealthBroken))
	g.Expect(meta.FindStatusCondition(sm.Status.Conditions, v1alpha1.ConditionHealthy)).NotTo(BeNil())
}

// eventReasons returns the reasons of the events.k8s.io Events regarding uid.
func eventReasons(g Gomega, h *harness, uid types.UID) []string {
	var list eventsv1.EventList
	g.Expect(k8s.List(h.ctx, &list)).To(Succeed())
	var out []string
	for _, e := range list.Items {
		if e.Regarding.UID == uid {
			out = append(out, e.Reason)
		}
	}
	return out
}
