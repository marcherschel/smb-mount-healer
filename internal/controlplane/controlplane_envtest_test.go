//go:build envtest

package controlplane_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/clock"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1 "github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/controlplane"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

const (
	eventually = 60 * time.Second
	poll       = 100 * time.Millisecond
)

var happyPath = []v1.Stage{
	v1.StagePending, v1.StagePlanning, v1.StageDraining, v1.StageReleasingMounts, v1.StageAwaitingUnstage,
	v1.StageQuarantining, v1.StageAwaitingUnstageCompletion, v1.StageRestartingDriver, v1.StageScalingUp,
	v1.StageVerifying, v1.StageFinalizing, v1.StageDone,
}

// waitStage waits for the single remediation of node to reach stage and returns it.
func waitStage(ctx context.Context, t *testing.T, node string, stage v1.Stage) v1.SmbMountRemediation {
	t.Helper()
	var out v1.SmbMountRemediation
	NewWithT(t).Eventually(func(g Gomega) {
		rs := remediations(ctx, node)
		g.Expect(rs).To(HaveLen(1))
		g.Expect(rs[0].Status.Stage).To(Equal(stage))
		out = rs[0]
	}).WithTimeout(eventually).WithPolling(poll).Should(Succeed())
	return out
}

// waitRemediation waits for the single remediation of node and returns it.
func waitRemediation(ctx context.Context, t *testing.T, node string) v1.SmbMountRemediation {
	t.Helper()
	var out v1.SmbMountRemediation
	NewWithT(t).Eventually(func(g Gomega) {
		rs := remediations(ctx, node)
		g.Expect(rs).To(HaveLen(1))
		out = rs[0]
	}).WithTimeout(eventually).WithPolling(poll).Should(Succeed())
	return out
}

// waitHeld waits until node's remediation waits on a held host task after
// draining (releasing_mounts or awaiting_unstage, depending on whether the
// worker still reported the drained pod's mount) and returns it.
func waitHeld(ctx context.Context, t *testing.T, node string) v1.SmbMountRemediation {
	t.Helper()
	var out v1.SmbMountRemediation
	NewWithT(t).Eventually(func(g Gomega) {
		rs := remediations(ctx, node)
		g.Expect(rs).To(HaveLen(1))
		out = rs[0]
		g.Expect(out.Status.Stage).To(BeElementOf(v1.StageReleasingMounts, v1.StageAwaitingUnstage))
		var task v1.SmbHostTask
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: v1.HostTaskName(out.Name, out.Status.Stage, 0)}, &task)).To(Succeed())
	}).WithTimeout(eventually).WithPolling(poll).Should(Succeed())
	return out
}

func outcomeOf(ctx context.Context, name string) func(g Gomega) v1.Outcome {
	return func(g Gomega) v1.Outcome {
		var r v1.SmbMountRemediation
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &r)).To(Succeed())
		g.Expect(r.Status.Stage).To(Equal(v1.StageDone))
		return r.Status.Outcome
	}
}

func TestHappyPathHealed(t *testing.T) {
	g := NewWithT(t)
	node := uniq("happy")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	w.breakNode(node)
	c := startCP(t, cpConfig(w.driverNS), controlplane.Options{})

	r := waitRemediation(w.ctx, t, node)
	g.Expect(r.Spec.Type).To(Equal(v1.RemediationTypeWorkload))
	g.Expect(r.Labels).To(HaveKeyWithValue(v1.LabelNode, node))
	g.Eventually(outcomeOf(w.ctx, r.Name)).WithTimeout(eventually).WithPolling(poll).Should(Equal(v1.OutcomeHealed))

	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	g.Expect(stages(&r)).To(Equal(happyPath))
	g.Expect(r.Status.Scope.Workloads).To(ConsistOf(HaveField("OriginalReplicas", int32(1))))
	g.Expect(r.Status.Driver.NewPodUID).NotTo(BeEmpty())
	g.Expect(r.Status.Verification).To(ConsistOf(HaveField("Result", v1.VerificationVerified)))
	g.Eventually(func() []string {
		_ = k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)
		return r.Finalizers
	}).WithTimeout(10 * time.Second).Should(BeEmpty())
	g.Expect(w.replicas()).To(Equal(int32(1)))

	// The SmbMount mirrors the outcome.
	g.Eventually(func(g Gomega) {
		sm := w.smbMount(node)
		g.Expect(sm.Status.Remediation).To(BeNil())
		g.Expect(sm.Status.LastRemediation).NotTo(BeNil())
		g.Expect(sm.Status.LastRemediation.Name).To(Equal(r.Name))
		g.Expect(sm.Status.LastRemediation.Outcome).To(Equal(v1.OutcomeHealed))
		g.Expect(meta.IsStatusConditionFalse(sm.Status.Conditions, v1.ConditionRemediating)).To(BeTrue())
		g.Expect(meta.IsStatusConditionFalse(sm.Status.Conditions, v1.ConditionDegraded)).To(BeTrue())
		g.Expect(sm.Status.RemediationsLastHour).To(Equal(int32(1)))
		g.Expect(sm.Status.Health).To(Equal(v1.HealthHealthy))
	}).WithTimeout(10 * time.Second).WithPolling(poll).Should(Succeed())

	// Host tasks: planning probe, force unmount, quarantine, verify probe.
	var tasks v1.SmbHostTaskList
	g.Expect(k8s.List(w.ctx, &tasks, client.MatchingLabels{v1.LabelRemediation: r.Name})).To(Succeed())
	g.Expect(len(tasks.Items)).To(BeNumerically(">=", 3))
	for _, task := range tasks.Items {
		g.Expect(task.Spec.DryRun).To(BeFalse())
	}

	m := c.m
	g.Expect(metricValue(m.DryRun)).To(BeZero(), "dry_run gauge reports enforcing")
	g.Expect(metricValue(m.RemediationsStarted.WithLabelValues(node, "Workload", metrics.ModeLive))).To(Equal(1.0))
	g.Expect(metricValue(m.Remediations.WithLabelValues(node, "Workload", metrics.ModeLive, "healed"))).To(Equal(1.0))
	g.Expect(metricValue(m.WorkloadActions.WithLabelValues("scale_down", metrics.ResultSuccess, metrics.ModeLive))).To(BeNumerically(">=", 1))
	g.Expect(metricValue(m.WorkloadActions.WithLabelValues("scale_up", metrics.ResultSuccess, metrics.ModeLive))).To(Equal(1.0))
	g.Expect(metricValue(m.WorkloadActions.WithLabelValues("driver_restart", metrics.ResultSuccess, metrics.ModeLive))).To(Equal(1.0))
	g.Expect(metricValue(m.WorkloadVerifications.WithLabelValues("verified"))).To(Equal(1.0))
	g.Expect(metricValue(m.RemediationTransitions.WithLabelValues("Workload", "verifying", "finalizing", "verified"))).To(Equal(1.0))
	g.Expect(seriesCount(m.RemediationStageDuration)).To(Equal(len(happyPath) - 1))
	g.Expect(seriesCount(m.RemediationsResumed)).To(BeZero())
	g.Expect(seriesCount(m.RemediationStage)).To(BeZero(), "stage gauge removed once done")
	g.Expect(metricValue(m.SmbMounts.WithLabelValues("Healthy"))).To(BeNumerically(">=", 1))
	// No second remediation follows.
	g.Consistently(func() int { return len(remediations(w.ctx, node)) }).WithTimeout(2 * time.Second).Should(Equal(1))

	// The worker deletes and later recreates the SmbMount: lastRemediation is re-mirrored.
	old := w.smbMount(node)
	w.mu.Lock()
	g.Expect(k8s.Delete(w.ctx, &old)).To(Succeed())
	g.Expect(k8s.Create(w.ctx, &v1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Labels: old.Labels}, Spec: old.Spec})).To(Succeed())
	delete(w.applied, old.Name)
	w.mu.Unlock()
	g.Eventually(func(g Gomega) {
		sm := w.smbMount(node)
		g.Expect(sm.UID).NotTo(Equal(old.UID))
		g.Expect(sm.Status.LastRemediation).NotTo(BeNil())
		g.Expect(sm.Status.LastRemediation.Name).To(Equal(r.Name))
	}).WithTimeout(10 * time.Second).WithPolling(poll).Should(Succeed())
}

// TestVerifyFailsWhileReplacementCannotMount: the share goes away during
// the episode (e2e S14). After the volume was unstaged the worker deletes
// the node's SmbMount, and the scaled-up replacement pod stays Pending
// because nothing can mount. That pod is still on the node: the workload
// must end "failed" (verify_failed), not "elsewhere" (healed).
func TestVerifyFailsWhileReplacementCannotMount(t *testing.T) {
	g := NewWithT(t)
	node := uniq("outage")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	w.breakNode(node)
	startCP(t, cpConfig(w.driverNS), controlplane.Options{})

	r := waitStage(w.ctx, t, node, v1.StageDraining)
	w.outage.Store(true)
	g.Eventually(func() bool {
		var sm v1.SmbMount
		return apierrors.IsNotFound(k8s.Get(w.ctx, client.ObjectKey{Name: v1.SmbMountName(node, w.pv)}, &sm))
	}).WithTimeout(eventually).WithPolling(poll).Should(BeTrue(), "SmbMount deleted once the volume is gone from the node")
	g.Eventually(outcomeOf(w.ctx, r.Name)).WithTimeout(eventually).WithPolling(poll).Should(Equal(v1.OutcomeVerifyFailed))
	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	g.Expect(stages(&r)).To(Equal(happyPath))
	g.Expect(r.Status.Verification).To(ConsistOf(HaveField("Result", v1.VerificationFailed)))
	pods := w.pods()
	g.Expect(pods).To(HaveKey("app-0"))
	g.Expect(pods["app-0"].Status.Phase).NotTo(Equal(corev1.PodRunning), "the replacement never started")
}

func TestResumeAfterRestart(t *testing.T) {
	g := NewWithT(t)
	node := uniq("resume")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	w.breakNode(node)
	w.hold.Store(true) // stalls the episode in awaiting_unstage (force-unmount task never completes)
	cfg := cpConfig(w.driverNS)
	a := startCP(t, cfg, controlplane.Options{})
	r := waitHeld(w.ctx, t, node)
	a.stop()
	g.Expect(seriesCount(a.m.RemediationsResumed)).To(BeZero())
	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	held := r.Status.Stage

	w.hold.Store(false)
	b := startCP(t, cfg, controlplane.Options{})
	g.Eventually(outcomeOf(w.ctx, r.Name)).WithTimeout(eventually).WithPolling(poll).Should(Equal(v1.OutcomeHealed))
	g.Expect(metricValue(b.m.RemediationsResumed.WithLabelValues(string(held)))).To(Equal(1.0))
	g.Expect(seriesCount(b.m.RemediationsStarted)).To(BeZero(), "no second remediation")
	g.Expect(remediations(w.ctx, node)).To(HaveLen(1))
	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	g.Expect(stages(&r)).To(Equal(happyPath))
	g.Expect(w.replicas()).To(Equal(int32(1)))
}

func TestFinalizerRollbackOnDelete(t *testing.T) {
	g := NewWithT(t)
	node := uniq("delete")
	w := newWorld(t, 2, node)
	w.waitPods(2)
	w.breakNode(node)
	w.noDrain.Store(true) // pods never leave: draining waits for SCALE_DOWN_WAIT
	cfg := cpConfig(w.driverNS)
	cfg.ControlPlane.ScaleDownWait = time.Minute
	c := startCP(t, cfg, controlplane.Options{})
	r := waitStage(w.ctx, t, node, v1.StageDraining)
	g.Eventually(w.replicas).WithTimeout(eventually).Should(BeZero())
	g.Expect(r.Finalizers).To(ContainElement(v1.FinalizerRestoreReplicas))

	g.Expect(k8s.Delete(w.ctx, &r)).To(Succeed())
	g.Eventually(func() bool {
		return apierrors.IsNotFound(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &v1.SmbMountRemediation{}))
	}).WithTimeout(eventually).WithPolling(poll).Should(BeTrue(), "finalizer removed after rollback")
	g.Expect(w.replicas()).To(Equal(int32(2)), "originalReplicas restored")
	g.Expect(metricValue(c.m.WorkloadActions.WithLabelValues("rollback", metrics.ResultSuccess, metrics.ModeLive))).To(Equal(1.0))
	g.Expect(metricValue(c.m.Remediations.WithLabelValues(node, "Workload", metrics.ModeLive, "rolled_back"))).To(Equal(1.0))
	g.Expect(metricValue(c.m.RemediationTransitions.WithLabelValues("Workload", "draining", "rolling_back", "deleted"))).To(Equal(1.0))
	g.Eventually(func(g Gomega) {
		sm := w.smbMount(node)
		g.Expect(sm.Status.LastRemediation).NotTo(BeNil())
		g.Expect(sm.Status.LastRemediation.Outcome).To(Equal(v1.OutcomeRolledBack))
		g.Expect(meta.IsStatusConditionTrue(sm.Status.Conditions, v1.ConditionDegraded)).To(BeTrue())
	}).WithTimeout(10 * time.Second).Should(Succeed())
}

func TestAbortAnnotation(t *testing.T) {
	g := NewWithT(t)
	node := uniq("abort")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	w.breakNode(node)
	w.noDrain.Store(true)
	cfg := cpConfig(w.driverNS)
	cfg.ControlPlane.ScaleDownWait = time.Minute
	startCP(t, cfg, controlplane.Options{})
	r := waitStage(w.ctx, t, node, v1.StageDraining)
	g.Eventually(w.replicas).WithTimeout(eventually).Should(BeZero())

	patch := client.MergeFrom(r.DeepCopy())
	r.Annotations = map[string]string{v1.AnnotationAbort: "true"}
	g.Expect(k8s.Patch(w.ctx, &r, patch)).To(Succeed())
	g.Eventually(outcomeOf(w.ctx, r.Name)).WithTimeout(eventually).WithPolling(poll).Should(Equal(v1.OutcomeRolledBack))
	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	g.Expect(meta.IsStatusConditionTrue(r.Status.Conditions, v1.ConditionAborted)).To(BeTrue())
	g.Expect(stages(&r)).To(ContainElements(v1.StageRollingBack, v1.StageFinalizing, v1.StageDone))
	g.Expect(r.Status.Transitions[len(r.Status.Transitions)-3].Reason).To(Equal("aborted"))
	g.Expect(w.replicas()).To(Equal(int32(1)))
	g.Eventually(func() []string {
		_ = k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)
		return r.Finalizers
	}).WithTimeout(10*time.Second).Should(BeEmpty(), "terminal objects drop the finalizer and stay for history")
}

func TestScopeConflictAcrossNodes(t *testing.T) {
	g := NewWithT(t)
	n1, n2 := uniq("conflict-a"), uniq("conflict-b")
	w := newWorld(t, 2, n1, n2) // app-0 on n1, app-1 on n2: one StatefulSet on both nodes
	w.waitPods(2)
	w.breakNode(n1)
	w.noDrain.Store(true)
	cfg := cpConfig(w.driverNS)
	cfg.ControlPlane.ScaleDownWait = time.Minute
	c := startCP(t, cfg, controlplane.Options{})
	r1 := waitStage(w.ctx, t, n1, v1.StageDraining)
	g.Expect(r1.Status.Scope.Workloads).To(ConsistOf(HaveField("WorkloadRef", v1.WorkloadRef{Kind: v1.WorkloadKindStatefulSet, Namespace: w.ns, Name: "app"})))

	w.breakNode(n2)
	g.Eventually(func(g Gomega) {
		sm := w.smbMount(n2)
		cond := meta.FindStatusCondition(sm.Status.Conditions, v1.ConditionRemediationSuppressed)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(cond.Reason).To(Equal(string(v1.SuppressionScopeConflict)))
	}).WithTimeout(eventually).WithPolling(poll).Should(Succeed())
	g.Expect(metricValue(c.m.RemediationSuppressed.WithLabelValues(string(v1.SuppressionScopeConflict)))).To(Equal(1.0))
	g.Consistently(func() int { return len(remediations(w.ctx, n2)) }).WithTimeout(2 * time.Second).Should(BeZero())
	g.Expect(metricValue(c.m.RemediationSuppressed.WithLabelValues(string(v1.SuppressionScopeConflict)))).To(Equal(1.0),
		"counted once per suppression, not per evaluation")
	// n1's mirror is untouched by n2's suppression and vice versa.
	sm1 := w.smbMount(n1)
	g.Expect(sm1.Status.Remediation).NotTo(BeNil())
	g.Expect(sm1.Status.Remediation.Name).To(Equal(r1.Name))
	g.Expect(meta.FindStatusCondition(sm1.Status.Conditions, v1.ConditionRemediationSuppressed)).To(BeNil(),
		"being remediated is not a suppression")
}

func TestBackoffSuppression(t *testing.T) {
	g := NewWithT(t)
	node := uniq("backoff")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	smName := v1.SmbMountName(node, w.pv)
	// History: a live remediation of this SmbMount finished seconds ago (backoff 1m).
	now := time.Now().UTC().Truncate(time.Second)
	past := &v1.SmbMountRemediation{
		ObjectMeta: metav1.ObjectMeta{Name: v1.RemediationName(node, now.Add(-2*time.Minute)), Labels: map[string]string{v1.LabelNode: node}},
		Spec: v1.SmbMountRemediationSpec{NodeName: node, Type: v1.RemediationTypeWorkload,
			Trigger: v1.RemediationTrigger{SmbMounts: []string{smName}, Reasons: []v1.TriggerReason{v1.TriggerWrongFSType}}},
	}
	g.Expect(k8s.Create(w.ctx, past)).To(Succeed())
	past.Status = v1.SmbMountRemediationStatus{
		Stage: v1.StageDone, Outcome: v1.OutcomeVerifyFailed,
		StartedTime: ptr.To(metav1.NewTime(now.Add(-time.Minute))), StageEnteredTime: ptr.To(metav1.NewTime(now.Add(-5 * time.Second))),
	}
	g.Expect(k8s.Status().Update(w.ctx, past)).To(Succeed())

	w.breakNode(node)
	c := startCP(t, cpConfig(w.driverNS), controlplane.Options{})
	g.Eventually(func(g Gomega) {
		sm := w.smbMount(node)
		cond := meta.FindStatusCondition(sm.Status.Conditions, v1.ConditionRemediationSuppressed)
		g.Expect(cond).NotTo(BeNil())
		g.Expect(cond.Reason).To(Equal(string(v1.SuppressionBackoff)))
		g.Expect(sm.Status.RemediationsLastHour).To(Equal(int32(1)))
		// The terminal remediation's mirror (another field group of the same manager) coexists.
		g.Expect(sm.Status.LastRemediation).NotTo(BeNil())
		g.Expect(sm.Status.LastRemediation.Outcome).To(Equal(v1.OutcomeVerifyFailed))
		g.Expect(meta.IsStatusConditionTrue(sm.Status.Conditions, v1.ConditionDegraded)).To(BeTrue())
	}).WithTimeout(eventually).WithPolling(poll).Should(Succeed())
	g.Consistently(func() int { return len(remediations(w.ctx, node)) }).WithTimeout(2 * time.Second).Should(Equal(1))
	g.Expect(metricValue(c.m.RemediationSuppressed.WithLabelValues(string(v1.SuppressionBackoff)))).To(Equal(1.0))
	g.Expect(seriesCount(c.m.RemediationsStarted)).To(BeZero())
}

// noWatch adapts a client.Client to client.WithWatch for the interceptor.
type noWatch struct{ client.Client }

func (noWatch) Watch(context.Context, client.ObjectList, ...client.ListOption) (watch.Interface, error) {
	return nil, errors.New("watch not supported")
}

// mutationCounter counts scale-subresource writes and pod deletions.
func mutationCounter(n *atomic.Int64) func(client.Client) client.Client {
	return func(c client.Client) client.Client {
		return interceptor.NewClient(noWatch{c}, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if sub == "scale" {
					n.Add(1)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
				if sub == "scale" {
					n.Add(1)
				}
				return c.SubResource(sub).Patch(ctx, obj, p, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					n.Add(1)
				}
				return c.Delete(ctx, obj, opts...)
			},
		})
	}
}

func TestDryRun(t *testing.T) {
	g := NewWithT(t)
	node := uniq("dry")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	w.breakNode(node)
	pods := w.pods()
	var writes atomic.Int64
	cfg := cpConfig(w.driverNS)
	cfg.ControlPlane.DryRun = true
	c := startCP(t, cfg, controlplane.Options{WrapClient: mutationCounter(&writes)})

	r := waitRemediation(w.ctx, t, node)
	g.Expect(r.Spec.DryRun).To(BeTrue())
	g.Eventually(outcomeOf(w.ctx, r.Name)).WithTimeout(eventually).WithPolling(poll).Should(Equal(v1.OutcomeDryRun))
	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	g.Expect(stages(&r)).To(Equal(happyPath), "dry run walks every stage")

	g.Expect(writes.Load()).To(BeZero(), "no scale and no pod delete in dry run")
	g.Expect(w.replicas()).To(Equal(int32(1)))
	g.Expect(w.pods()["app-0"].UID).To(Equal(pods["app-0"].UID))
	var tasks v1.SmbHostTaskList
	g.Expect(k8s.List(w.ctx, &tasks, client.MatchingLabels{v1.LabelRemediation: r.Name})).To(Succeed())
	g.Expect(tasks.Items).NotTo(BeEmpty())
	for _, task := range tasks.Items {
		g.Expect(task.Spec.DryRun).To(BeTrue(), task.Name)
	}
	g.Expect(metricValue(c.m.DryRun)).To(Equal(1.0), "dry_run gauge reports dry run")
	g.Expect(metricValue(c.m.RemediationsStarted.WithLabelValues(node, "Workload", metrics.ModeDryRun))).To(Equal(1.0))
	g.Expect(metricValue(c.m.Remediations.WithLabelValues(node, "Workload", metrics.ModeDryRun, "dry_run"))).To(Equal(1.0))
	for _, a := range []string{"scale_down", "scale_reassert", "scale_up", "driver_restart", "rollback"} {
		g.Expect(metricValue(c.m.WorkloadActions.WithLabelValues(a, metrics.ResultSuccess, metrics.ModeLive))).To(BeZero(), a)
	}
	sm := w.smbMount(node)
	g.Expect(sm.Status.LastRemediation).NotTo(BeNil())
	g.Expect(sm.Status.LastRemediation.Outcome).To(Equal(v1.OutcomeDryRun))
}

func TestLeaderHandover(t *testing.T) {
	g := NewWithT(t)
	node := uniq("leader")
	w := newWorld(t, 1, node)
	w.waitPods(1)
	w.breakNode(node)
	w.hold.Store(true)
	cfg := cpConfig(w.driverNS)
	cfg.ControlPlane.LeaderElection = true
	cfg.ControlPlane.LeaderElectionNamespace = w.ns
	le := controlplane.Options{LeaseDuration: 4 * time.Second, RenewDeadline: 3 * time.Second, RetryPeriod: 500 * time.Millisecond,
		KeepLeaseOnCancel: true} // the old leader "crashes": the successor waits for the Lease to expire
	a := startCP(t, cfg, le)
	g.Eventually(a.mgr.Elected()).WithTimeout(eventually).Should(BeClosed())
	b := startCP(t, cfg, le)
	r := waitHeld(w.ctx, t, node)
	g.Consistently(b.mgr.Elected()).WithTimeout(time.Second).ShouldNot(BeClosed())
	g.Expect(seriesCount(b.m.RemediationsResumed)).To(BeZero(), "a standby does not reconcile")

	a.stop()
	g.Expect(k8s.Get(w.ctx, client.ObjectKey{Name: r.Name}, &r)).To(Succeed())
	held := r.Status.Stage
	w.hold.Store(false)
	g.Eventually(b.mgr.Elected()).WithTimeout(eventually).Should(BeClosed())
	g.Eventually(outcomeOf(w.ctx, r.Name)).WithTimeout(eventually).WithPolling(poll).Should(Equal(v1.OutcomeHealed))
	g.Expect(metricValue(b.m.RemediationsResumed.WithLabelValues(string(held)))).To(Equal(1.0))
	g.Expect(metricValue(a.m.RemediationsStarted.WithLabelValues(node, "Workload", metrics.ModeLive))).To(Equal(1.0))
	g.Expect(seriesCount(b.m.RemediationsStarted)).To(BeZero())
	g.Expect(remediations(w.ctx, node)).To(HaveLen(1))
	g.Expect(w.replicas()).To(Equal(int32(1)))
}

// writeCounter counts SmbMount status applies.
func writeCounter(c client.WithWatch, n *atomic.Int64) client.WithWatch {
	return interceptor.NewClient(c, interceptor.Funcs{
		SubResourceApply: func(ctx context.Context, c client.Client, sub string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
			n.Add(1)
			return c.SubResource(sub).Apply(ctx, obj, opts...)
		},
	})
}

// TestStatusWriterNeverDropsFields proves that the trigger and mirror field
// groups, both applied by the control-plane field manager, never remove each
// other's fields, even when a caller's view of the SmbMount is stale.
func TestStatusWriterNeverDropsFields(t *testing.T) {
	g := NewWithT(t)
	ctx, _ := setup(t)
	var applies atomic.Int64
	c := writeCounter(k8s, &applies)
	e, _ := newExecutor(t, c, false)
	fc := clocktesting.NewFakeClock(time.Now().UTC().Truncate(time.Second))
	sw := controlplane.NewSmbMountStatusWriter(k8s, e, fc)
	node := uniq("ssa")
	name := v1.SmbMountName(node, "pv")
	g.Expect(k8s.Create(ctx, &v1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.SmbMountSpec{NodeName: node, PVName: "pv", Share: "//s/pv", VolumeHandle: "pv", GlobalmountPath: gmPath("pv")}})).To(Succeed())
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), &v1.SmbMount{ObjectMeta: metav1.ObjectMeta{Name: name}}) })
	get := func() v1.SmbMountStatus {
		var sm v1.SmbMount
		g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &sm)).To(Succeed())
		return sm.Status
	}
	suppressed := &metav1.Condition{Type: v1.ConditionRemediationSuppressed, Status: metav1.ConditionTrue,
		Reason: string(v1.SuppressionBackoff), Message: "backoff"}

	// 1. trigger group
	newly, err := sw.ApplyTrigger(ctx, name, controlplane.TriggerFields{RemediationsLastHour: 2, Suppressed: suppressed})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(newly).To(BeTrue())
	newly, err = sw.ApplyTrigger(ctx, name, controlplane.TriggerFields{RemediationsLastHour: 2, Suppressed: suppressed})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(newly).To(BeFalse(), "an unchanged suppression is not new")
	// 2. mirror group, built from a stale view that knows nothing of the trigger group (as Step would emit it)
	stamp := metav1.NewTime(fc.Now())
	g.Expect(sw.ApplyMirror(ctx, command.PatchSmbMountRemediationMirror{
		SmbMount:    name,
		Remediation: &v1.ActiveRemediation{Active: true, Name: node + "-1", Stage: v1.StageDraining, StageEnteredTime: &stamp},
		Conditions:  []metav1.Condition{{Type: v1.ConditionRemediating, Status: metav1.ConditionTrue, Reason: "draining", Message: "x", LastTransitionTime: stamp}},
	})).To(Succeed())
	st := get()
	g.Expect(st.RemediationsLastHour).To(Equal(int32(2)), "mirror kept remediationsLastHour")
	g.Expect(meta.IsStatusConditionTrue(st.Conditions, v1.ConditionRemediationSuppressed)).To(BeTrue(), "mirror kept RemediationSuppressed")
	g.Expect(st.Remediation.Name).To(Equal(node + "-1"))

	// 3. trigger group again: the mirror group survives, the suppression is released.
	_, err = sw.ApplyTrigger(ctx, name, controlplane.TriggerFields{RemediationsLastHour: 3})
	g.Expect(err).NotTo(HaveOccurred())
	st = get()
	g.Expect(st.Remediation).NotTo(BeNil(), "trigger kept remediation")
	g.Expect(st.Remediation.Stage).To(Equal(v1.StageDraining))
	g.Expect(meta.IsStatusConditionTrue(st.Conditions, v1.ConditionRemediating)).To(BeTrue(), "trigger kept Remediating")
	g.Expect(meta.FindStatusCondition(st.Conditions, v1.ConditionRemediationSuppressed)).To(BeNil())
	g.Expect(st.RemediationsLastHour).To(Equal(int32(3)))

	// 4. terminal mirror: remediation released, lastRemediation + Degraded set, trigger group kept.
	g.Expect(sw.ApplyMirror(ctx, command.PatchSmbMountRemediationMirror{
		SmbMount:        name,
		LastRemediation: &v1.LastRemediation{Name: node + "-1", Outcome: v1.OutcomeVerifyFailed, StartedTime: &stamp, FinishedTime: &stamp},
		Conditions: []metav1.Condition{
			{Type: v1.ConditionRemediating, Status: metav1.ConditionFalse, Reason: "Idle", Message: "idle", LastTransitionTime: stamp},
			{Type: v1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: "verify_failed", Message: "x", LastTransitionTime: stamp},
		},
	})).To(Succeed())
	st = get()
	g.Expect(st.Remediation).To(BeNil())
	g.Expect(st.LastRemediation.Outcome).To(Equal(v1.OutcomeVerifyFailed))
	g.Expect(st.RemediationsLastHour).To(Equal(int32(3)))
	g.Expect(meta.IsStatusConditionTrue(st.Conditions, v1.ConditionDegraded)).To(BeTrue())

	// 5. trigger again with the same values: nothing is written; then a new suppression keeps the mirror.
	before := applies.Load()
	_, err = sw.ApplyTrigger(ctx, name, controlplane.TriggerFields{RemediationsLastHour: 3})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(applies.Load()).To(Equal(before), "unchanged fields are not re-applied")
	fc.Step(time.Minute)
	newly, err = sw.ApplyTrigger(ctx, name, controlplane.TriggerFields{RemediationsLastHour: 3, Suppressed: suppressed})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(newly).To(BeTrue())
	st = get()
	g.Expect(st.LastRemediation.Outcome).To(Equal(v1.OutcomeVerifyFailed))
	g.Expect(meta.IsStatusConditionTrue(st.Conditions, v1.ConditionDegraded)).To(BeTrue())
	g.Expect(meta.IsStatusConditionFalse(st.Conditions, v1.ConditionRemediating)).To(BeTrue())
	g.Expect(meta.FindStatusCondition(st.Conditions, v1.ConditionRemediationSuppressed).LastTransitionTime.Time).
		To(BeTemporally("==", fc.Now()))
	g.Expect(managedStatusFields(mustGet(ctx, t, name).ManagedFields, v1.FieldManagerControlPlane)).To(And(
		ContainSubstring(`"f:lastRemediation"`), ContainSubstring(`"f:remediationsLastHour"`),
		ContainSubstring(`"k:{\"type\":\"Degraded\"}"`), ContainSubstring(`"k:{\"type\":\"RemediationSuppressed\"}"`),
	))
}

func mustGet(ctx context.Context, t *testing.T, name string) *v1.SmbMount {
	t.Helper()
	var sm v1.SmbMount
	if err := k8s.Get(ctx, client.ObjectKey{Name: name}, &sm); err != nil {
		t.Fatal(err)
	}
	return &sm
}

func TestGCRetention(t *testing.T) {
	g := NewWithT(t)
	ctx, _ := setup(t)
	node, other := uniq("gc"), uniq("gc-other")
	now := time.Now().UTC().Truncate(time.Second)
	fc := clocktesting.NewFakeClock(now)
	mk := func(n, name string, stage v1.Stage, finished time.Time) {
		r := &v1.SmbMountRemediation{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{v1.LabelNode: n}},
			Spec: v1.SmbMountRemediationSpec{NodeName: n, Type: v1.RemediationTypeSweep,
				Trigger: v1.RemediationTrigger{SmbMounts: []string{n + ".pv"}}}}
		g.Expect(k8s.Create(ctx, r)).To(Succeed())
		r.Status = v1.SmbMountRemediationStatus{Stage: stage, StageEnteredTime: ptr.To(metav1.NewTime(finished)),
			StartedTime: ptr.To(metav1.NewTime(finished.Add(-time.Minute)))}
		if stage == v1.StageDone {
			r.Status.Outcome = v1.OutcomeHealed
		}
		g.Expect(k8s.Status().Update(ctx, r)).To(Succeed())
		t.Cleanup(func() { _ = k8s.Delete(context.Background(), r) })
	}
	// 14 terminal remediations finished 0h, 3h, ..., 39h ago.
	for i := 0; i < 14; i++ {
		mk(node, v1.RemediationName(node, now.Add(-time.Duration(i)*3*time.Hour)), v1.StageDone, now.Add(-time.Duration(i)*3*time.Hour))
	}
	mk(node, node+"-active", v1.StageDraining, now.Add(-48*time.Hour))
	mk(other, other+"-old", v1.StageDone, now.Add(-72*time.Hour))

	kc, err := kube.NewClientCluster(kube.ClientOptions{Client: k8s, DriverNamespace: "kube-system",
		DriverSelector: labels.SelectorFromSet(labels.Set{"app": "csi-smb-node"})})
	g.Expect(err).NotTo(HaveOccurred())
	e, _ := newExecutor(t, k8s, false)
	cfg := cpConfig("kube-system").ControlPlane
	eng, err := controlplane.NewEngine(controlplane.EngineOptions{Client: k8s, Cluster: kc, Executor: e, Clock: clock.PassiveClock(fc), Config: cfg})
	g.Expect(err).NotTo(HaveOccurred())

	res, err := eng.ReconcileGC(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(time.Hour), "nothing left to expire within the hour cap")
	left := remediations(ctx, node)
	names := map[string]bool{}
	for _, r := range left {
		names[r.Name] = true
	}
	g.Expect(left).To(HaveLen(11), "newest 10 terminal (incl. 27h old) + the active one")
	for i := 0; i < 10; i++ {
		g.Expect(names).To(HaveKey(v1.RemediationName(node, now.Add(-time.Duration(i)*3*time.Hour))))
	}
	g.Expect(names).To(HaveKey(node + "-active"))
	g.Expect(remediations(ctx, other)).To(HaveLen(1), "other nodes are untouched")

	// With fewer retained, the 27h and 24h ones go, the next one expires in 2.5h (capped to 1h).
	cfg.RemediationRetainMinPerNode = 2
	eng, err = controlplane.NewEngine(controlplane.EngineOptions{Client: k8s, Cluster: kc, Executor: e, Clock: fc, Config: cfg})
	g.Expect(err).NotTo(HaveOccurred())
	fc.Step(30 * time.Minute)
	res, err = eng.ReconcileGC(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(remediations(ctx, node)).To(HaveLen(9))
	g.Expect(res.RequeueAfter).To(Equal(time.Hour))
}

// TestTerminalRemediationRefreshesHostTaskPhases: the worker marks a ProbeNow
// task Succeeded only after the probe round reflecting it is published, and
// that round can be what lets verifying finish (e2e S5 on kernel 6.17). The
// remediation is then done while status.hostTasks still holds the
// write-ahead ref without a phase; the task's later phase must still be
// copied in.
func TestTerminalRemediationRefreshesHostTaskPhases(t *testing.T) {
	g := NewWithT(t)
	ctx, _ := setup(t)
	node := uniq("terminal-task")
	now := time.Now().UTC().Truncate(time.Second)
	r := &v1.SmbMountRemediation{ObjectMeta: metav1.ObjectMeta{Name: v1.RemediationName(node, now), Labels: map[string]string{v1.LabelNode: node}},
		Spec: v1.SmbMountRemediationSpec{NodeName: node, Type: v1.RemediationTypeWorkload,
			Trigger: v1.RemediationTrigger{SmbMounts: []string{node + ".pv"}}}}
	g.Expect(k8s.Create(ctx, r)).To(Succeed())
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), r) })
	taskName := v1.HostTaskName(r.Name, v1.StageVerifying, 1)
	r.Status = v1.SmbMountRemediationStatus{Stage: v1.StageDone, Outcome: v1.OutcomeHealed,
		StageEnteredTime: ptr.To(metav1.NewTime(now)), StartedTime: ptr.To(metav1.NewTime(now.Add(-time.Minute))),
		HostTasks: []v1.HostTaskRef{{Name: taskName, Stage: v1.StageVerifying}}}
	g.Expect(k8s.Status().Update(ctx, r)).To(Succeed())

	task := &v1.SmbHostTask{
		ObjectMeta: metav1.ObjectMeta{Name: taskName,
			Labels:          map[string]string{v1.LabelRemediation: r.Name, v1.LabelNode: node},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1.GroupVersion.String(), Kind: "SmbMountRemediation", Name: r.Name, UID: r.UID}}},
		Spec: v1.SmbHostTaskSpec{NodeName: node, Actions: []v1.HostAction{{ID: "probe-000", Type: v1.HostActionProbeNow, Path: "/var/lib/kubelet/pods/u/volumes/kubernetes.io~csi/pv/mount"}}},
	}
	g.Expect(k8s.Create(ctx, task)).To(Succeed())
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), task) })
	task.Status = v1.SmbHostTaskStatus{Phase: v1.HostTaskSucceeded,
		Results: []v1.HostActionResult{{ID: "probe-000", Result: v1.HostActionSucceeded, Time: metav1.NewTime(now)}}}
	g.Expect(k8s.Status().Update(ctx, task)).To(Succeed())

	kc, err := kube.NewClientCluster(kube.ClientOptions{Client: k8s, DriverNamespace: "kube-system",
		DriverSelector: labels.SelectorFromSet(labels.Set{"app": "csi-smb-node"})})
	g.Expect(err).NotTo(HaveOccurred())
	e, _ := newExecutor(t, k8s, false)
	eng, err := controlplane.NewEngine(controlplane.EngineOptions{Client: k8s, Cluster: kc, Executor: e,
		Clock: clocktesting.NewFakeClock(now.Add(time.Second)), Config: cpConfig("kube-system").ControlPlane})
	g.Expect(err).NotTo(HaveOccurred())

	_, err = eng.ReconcileRemediation(ctx, r.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: r.Name}, r)).To(Succeed())
	g.Expect(r.Status.Stage).To(Equal(v1.StageDone))
	g.Expect(r.Status.HostTasks).To(ConsistOf(v1.HostTaskRef{Name: taskName, Stage: v1.StageVerifying, Phase: v1.HostTaskSucceeded}))
}
