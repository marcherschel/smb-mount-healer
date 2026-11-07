//go:build envtest

package controlplane_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	ac "github.com/marcherschel/smb-mount-healer/api/v1alpha1/applyconfiguration/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/command"
	"github.com/marcherschel/smb-mount-healer/internal/controlplane"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
	"github.com/marcherschel/smb-mount-healer/internal/metrics"
)

var (
	scheme *runtime.Scheme
	k8s    client.WithWatch
	// restConfig is the envtest API server's config (managers in controlplane_envtest_test.go).
	restConfig *rest.Config
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic("starting envtest (run via `make test-envtest`): " + err.Error())
	}
	restConfig = cfg
	if scheme, err = kube.NewScheme(); err != nil {
		panic(err)
	}
	if k8s, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func setup(t *testing.T) (context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "cp-test-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	return ctx, ns.Name
}

// countingClient wraps c and counts every mutating call.
func countingClient(c client.WithWatch, writes *atomic.Int64) client.WithWatch {
	inc := func() { writes.Add(1) }
	return interceptor.NewClient(c, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			inc()
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			inc()
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
			inc()
			return c.Patch(ctx, obj, p, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			inc()
			return c.Apply(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			inc()
			return c.Delete(ctx, obj, opts...)
		},
		DeleteAllOf: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteAllOfOption) error {
			inc()
			return c.DeleteAllOf(ctx, obj, opts...)
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object, opts ...client.SubResourceCreateOption) error {
			inc()
			return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			inc()
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
			inc()
			return c.SubResource(sub).Patch(ctx, obj, p, opts...)
		},
		SubResourceApply: func(ctx context.Context, c client.Client, sub string, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
			inc()
			return c.SubResource(sub).Apply(ctx, obj, opts...)
		},
	})
}

func newExecutor(t *testing.T, c client.Client, dryRun bool) (*controlplane.Executor, *metrics.ControlPlane) {
	t.Helper()
	kc, err := kube.NewClientCluster(kube.ClientOptions{
		Client: c, DriverNamespace: "kube-system",
		DriverSelector: labels.SelectorFromSet(labels.Set{"app": "csi-smb-node"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.NewControlPlane()
	e, err := controlplane.NewExecutor(controlplane.ExecutorOptions{Cluster: kc, Client: c, Metrics: m, DryRun: dryRun})
	if err != nil {
		t.Fatal(err)
	}
	return e, m
}

func workloadActions(m *metrics.ControlPlane, action command.WorkloadAction, result, mode string) float64 {
	var out dto.Metric
	if err := m.WorkloadActions.WithLabelValues(string(action), result, mode).Write(&out); err != nil {
		panic(err)
	}
	return out.GetCounter().GetValue()
}

// seriesCount returns the number of series in a collector.
func seriesCount(c interface {
	Collect(chan<- prometheus.Metric)
}) int {
	ch := make(chan prometheus.Metric, 64)
	c.Collect(ch)
	close(ch)
	return len(ch)
}

func newDeployment(ns, name string, replicas int32) *appsv1.Deployment {
	l := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Selector: &metav1.LabelSelector{MatchLabels: l},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}},
	}
}

func newPod(ns, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}
}

func newRemediation(ctx context.Context, g Gomega, name string) *v1.SmbMountRemediation {
	r := &v1.SmbMountRemediation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.SmbMountRemediationSpec{
			NodeName: "node-a", Type: v1.RemediationTypeWorkload,
			Trigger: v1.RemediationTrigger{SmbMounts: []string{"node-a.pv"}, Reasons: []v1.TriggerReason{v1.TriggerWrongFSType}},
		},
	}
	g.Expect(k8s.Create(ctx, r)).To(Succeed())
	return r
}

func TestScaleAndDeletePodLive(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	var writes atomic.Int64
	e, m := newExecutor(t, countingClient(k8s, &writes), false)

	d := newDeployment(ns, "web", 2)
	g.Expect(k8s.Create(ctx, d)).To(Succeed())
	ref := v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: ns, Name: "web"}

	scale := command.ScaleWorkload{Workload: ref, Replicas: 0, Action: command.ActionScaleDown}
	g.Expect(e.ExecuteCluster(ctx, scale)).To(Succeed())
	g.Expect(writes.Load()).To(BeNumerically(">", 0))
	g.Expect(e.ExecuteCluster(ctx, scale)).To(Succeed(), "re-executing is idempotent")
	g.Expect(workloadActions(m, command.ActionScaleDown, metrics.ResultSuccess, metrics.ModeLive)).To(Equal(1.0),
		"a no-op re-execution is not counted")
	var got appsv1.Deployment
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(d), &got)).To(Succeed())
	g.Expect(*got.Spec.Replicas).To(BeZero())

	gone := command.ScaleWorkload{Workload: v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: ns, Name: "gone"}, Replicas: 1, Action: command.ActionRollback}
	g.Expect(e.ExecuteCluster(ctx, gone)).To(Succeed(), "a vanished workload is skipped")

	pod := newPod(ns, "driver")
	g.Expect(k8s.Create(ctx, pod)).To(Succeed())
	del := command.DeletePod{Namespace: ns, Name: "driver", UID: "wrong", Action: command.ActionDriverRestart}
	g.Expect(e.ExecuteCluster(ctx, del)).To(Succeed())
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})).To(Succeed(), "wrong UID must not delete")
	g.Expect(workloadActions(m, command.ActionDriverRestart, metrics.ResultSuccess, metrics.ModeLive)).To(BeZero())
	del.UID = pod.UID
	g.Expect(e.ExecuteCluster(ctx, del)).To(Succeed())
	g.Expect(e.ExecuteCluster(ctx, del)).To(Succeed())
	g.Expect(workloadActions(m, command.ActionDriverRestart, metrics.ResultSuccess, metrics.ModeLive)).To(Equal(1.0))
	g.Expect(apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())
}

func TestDryRunExecutorIssuesNoWorkloadWrites(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	var writes atomic.Int64
	e, m := newExecutor(t, countingClient(k8s, &writes), true)

	d := newDeployment(ns, "web", 2)
	g.Expect(k8s.Create(ctx, d)).To(Succeed())
	pod := newPod(ns, "driver")
	g.Expect(k8s.Create(ctx, pod)).To(Succeed())

	ref := v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: ns, Name: "web"}
	for _, cmd := range []command.ClusterCommand{
		command.ScaleWorkload{Workload: ref, Replicas: 0, Action: command.ActionScaleDown},
		command.ScaleWorkload{Workload: ref, Replicas: 2, Action: command.ActionScaleUp},
		command.ScaleWorkload{Workload: ref, Replicas: 0, Action: command.ActionRollback},
		command.DeletePod{Namespace: ns, Name: "driver", UID: pod.UID, Action: command.ActionDriverRestart},
	} {
		g.Expect(e.ExecuteCluster(ctx, cmd)).To(Succeed(), cmd.Key())
	}
	g.Expect(writes.Load()).To(BeZero(), "dry run must not issue any mutating call")

	var got appsv1.Deployment
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(d), &got)).To(Succeed())
	g.Expect(*got.Spec.Replicas).To(Equal(int32(2)))
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})).To(Succeed())

	g.Expect(workloadActions(m, command.ActionScaleDown, metrics.ResultSuccess, metrics.ModeDryRun)).To(Equal(1.0))
	g.Expect(workloadActions(m, command.ActionDriverRestart, metrics.ResultSuccess, metrics.ModeDryRun)).To(Equal(1.0))
	g.Expect(seriesCount(m.WorkloadActions)).To(Equal(4))
	g.Expect(workloadActions(m, command.ActionScaleDown, metrics.ResultSuccess, metrics.ModeLive)).To(BeZero())

	// Host tasks are still created, but always with spec.dryRun.
	r := newRemediation(ctx, g, "node-a-dry")
	g.Expect(e.ExecuteCluster(ctx, command.CreateHostTask{
		Name: "node-a-dry-planning-1", NodeName: "node-a", Owner: command.OwnerRef{Name: r.Name, UID: r.UID}, DryRun: false,
		Actions: []v1.HostAction{{ID: "p", Type: v1.HostActionProbeNow, Path: "/var/lib/kubelet"}},
	})).To(Succeed())
	var task v1.SmbHostTask
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: "node-a-dry-planning-1"}, &task)).To(Succeed())
	g.Expect(task.Spec.DryRun).To(BeTrue())
}

func TestHostTaskLifecycle(t *testing.T) {
	g := NewWithT(t)
	ctx, _ := setup(t)
	e, _ := newExecutor(t, k8s, false)
	r := newRemediation(ctx, g, "node-a-100")

	create := command.CreateHostTask{
		Name: v1.HostTaskName(r.Name, v1.StageQuarantining, 1), NodeName: "node-a",
		Owner: command.OwnerRef{Name: r.Name, UID: r.UID},
		Actions: []v1.HostAction{
			{ID: "u1", Type: v1.HostActionForceUnmount, Path: "/var/lib/kubelet/pods/u/x/mount", OnlyIf: v1.OnlyIfNotHealthyMount},
			{ID: "q1", Type: v1.HostActionQuarantine, Path: "/var/lib/kubelet/pods/u/x/mount", PV: "pv", OnlyIf: v1.OnlyIfNotMountpoint},
		},
	}
	g.Expect(e.ExecuteCluster(ctx, create)).To(Succeed())
	var first v1.SmbHostTask
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: create.Name}, &first)).To(Succeed())
	g.Expect(e.ExecuteCluster(ctx, create)).To(Succeed(), "AlreadyExists counts as success")
	var second v1.SmbHostTask
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: create.Name}, &second)).To(Succeed())
	g.Expect(second.ResourceVersion).To(Equal(first.ResourceVersion), "re-creating must not modify the task")

	g.Expect(second.Labels).To(Equal(map[string]string{v1.LabelNode: "node-a", v1.LabelRemediation: r.Name}))
	g.Expect(second.Spec.NodeName).To(Equal("node-a"))
	g.Expect(second.Spec.DryRun).To(BeFalse())
	g.Expect(second.Spec.Actions).To(Equal(create.Actions))
	owner := metav1.GetControllerOf(&second)
	g.Expect(owner).NotTo(BeNil())
	g.Expect(*owner).To(Equal(metav1.OwnerReference{
		APIVersion: v1.GroupVersion.String(), Kind: "SmbMountRemediation", Name: r.Name, UID: r.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}))

	g.Expect(e.ExecuteCluster(ctx, command.CreateHostTask{Name: "no-owner", NodeName: "node-a", Actions: create.Actions})).NotTo(Succeed())

	del := command.DeleteHostTask{Name: create.Name}
	g.Expect(e.ExecuteCluster(ctx, del)).To(Succeed())
	g.Expect(apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKey{Name: create.Name}, &v1.SmbHostTask{}))).To(BeTrue())
	g.Expect(e.ExecuteCluster(ctx, del)).To(Succeed(), "NotFound counts as success")
}

func TestSmbMountMirrorCoexistsWithWorker(t *testing.T) {
	g := NewWithT(t)
	ctx, _ := setup(t)
	e, _ := newExecutor(t, k8s, false)
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))

	name := v1.SmbMountName("node-m", "pv-m")
	sm := &v1.SmbMount{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.SmbMountSpec{NodeName: "node-m", PVName: "pv-m", Share: "//samba/m", VolumeHandle: "pv-m",
			GlobalmountPath: "/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/abc/globalmount"},
	}
	g.Expect(k8s.Create(ctx, sm)).To(Succeed())

	workerApply := func(health v1.Health, cond metav1.ConditionStatus) {
		w := ac.SmbMount(name).WithStatus(ac.SmbMountStatus().
			WithHealth(health).WithReason(v1.ReasonWrongFSType).WithLastProbeTime(now).
			WithGlobalmount(ac.GlobalmountStatus().WithMounted(true).WithFSType("xfs").WithDirExists(true).WithEmpty(false)).
			WithPodMounts(ac.PodMountStatus().WithNamespace("media").WithPod("app-0").WithUID("uid-1").WithMounted(true)).
			WithPendingQuarantines(1).
			WithConditions(metav1ac.Condition().WithType(v1.ConditionHealthy).WithStatus(cond).WithReason("Probe").
				WithMessage("probe").WithLastTransitionTime(now)))
		g.Expect(k8s.Status().Apply(ctx, w, client.FieldOwner(v1.FieldManagerWorker), client.ForceOwnership)).To(Succeed())
	}
	workerApply(v1.HealthBroken, metav1.ConditionFalse)
	var before v1.SmbMount
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &before)).To(Succeed())

	g.Expect(e.ExecuteCluster(ctx, command.PatchSmbMountRemediationMirror{
		SmbMount:             name,
		Remediation:          &v1.ActiveRemediation{Active: true, Name: "node-m-1", Stage: v1.StageDraining, StageEnteredTime: &now},
		LastRemediation:      &v1.LastRemediation{Name: "node-m-0", Outcome: v1.OutcomeHealed, StartedTime: &now, FinishedTime: &now},
		RemediationsLastHour: ptr.To[int32](2),
		Conditions: []metav1.Condition{
			{Type: v1.ConditionRemediating, Status: metav1.ConditionTrue, Reason: "Draining", Message: "remediation node-m-1"},
		},
	})).To(Succeed())

	var got v1.SmbMount
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
	// Worker fields untouched.
	g.Expect(got.Status.Health).To(Equal(before.Status.Health))
	g.Expect(got.Status.Reason).To(Equal(before.Status.Reason))
	g.Expect(got.Status.Globalmount).To(Equal(before.Status.Globalmount))
	g.Expect(got.Status.PodMounts).To(Equal(before.Status.PodMounts))
	g.Expect(got.Status.PendingQuarantines).To(Equal(int32(1)))
	g.Expect(meta.FindStatusCondition(got.Status.Conditions, v1.ConditionHealthy)).To(Equal(meta.FindStatusCondition(before.Status.Conditions, v1.ConditionHealthy)))
	// Control-plane fields set.
	g.Expect(got.Status.Remediation).NotTo(BeNil())
	g.Expect(got.Status.Remediation.Active).To(BeTrue())
	g.Expect(got.Status.Remediation.Name).To(Equal("node-m-1"))
	g.Expect(got.Status.Remediation.Stage).To(Equal(v1.StageDraining))
	g.Expect(got.Status.Remediation.StageEnteredTime.Time).To(BeTemporally("==", now.Time))
	g.Expect(got.Status.LastRemediation.Outcome).To(Equal(v1.OutcomeHealed))
	g.Expect(got.Status.RemediationsLastHour).To(Equal(int32(2)))
	rem := meta.FindStatusCondition(got.Status.Conditions, v1.ConditionRemediating)
	g.Expect(rem).NotTo(BeNil())
	g.Expect(rem.LastTransitionTime.IsZero()).To(BeFalse(), "unset LastTransitionTime is filled")
	remLTT := rem.LastTransitionTime

	// The control plane's managed fields contain only its own fields.
	g.Expect(managedStatusFields(got.ManagedFields, v1.FieldManagerControlPlane)).To(And(
		ContainSubstring(`"f:remediation"`), ContainSubstring(`"f:lastRemediation"`), ContainSubstring(`"f:remediationsLastHour"`),
		ContainSubstring(`"k:{\"type\":\"Remediating\"}"`),
		Not(ContainSubstring(`"f:health"`)), Not(ContainSubstring(`"f:podMounts"`)), Not(ContainSubstring(`"f:globalmount"`)),
		Not(ContainSubstring(`Healthy`)), Not(ContainSubstring(`"f:pendingQuarantines"`)),
	))

	// The worker re-applies; the control plane's fields survive.
	workerApply(v1.HealthHealthy, metav1.ConditionTrue)
	// Same Remediating status without a time keeps the live LastTransitionTime;
	// Remediation omitted releases it; a new control-plane condition appears.
	g.Expect(e.ExecuteCluster(ctx, command.PatchSmbMountRemediationMirror{
		SmbMount:        name,
		LastRemediation: &v1.LastRemediation{Name: "node-m-1", Outcome: v1.OutcomeVerifyFailed},
		Conditions: []metav1.Condition{
			{Type: v1.ConditionRemediating, Status: metav1.ConditionTrue, Reason: "Verifying", Message: "x"},
			{Type: v1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: "VerifyFailed", Message: "x", LastTransitionTime: now},
		},
	})).To(Succeed())
	got = v1.SmbMount{}
	g.Expect(k8s.Get(ctx, client.ObjectKey{Name: name}, &got)).To(Succeed())
	g.Expect(got.Status.Health).To(Equal(v1.HealthHealthy))
	g.Expect(got.Status.PodMounts).To(HaveLen(1))
	g.Expect(got.Status.Remediation).To(BeNil(), "omitted fields are released")
	g.Expect(got.Status.RemediationsLastHour).To(BeZero())
	g.Expect(got.Status.LastRemediation.Name).To(Equal("node-m-1"))
	g.Expect(meta.IsStatusConditionTrue(got.Status.Conditions, v1.ConditionHealthy)).To(BeTrue())
	g.Expect(meta.FindStatusCondition(got.Status.Conditions, v1.ConditionRemediating).LastTransitionTime.Time).To(BeTemporally("==", remLTT.Time))
	g.Expect(meta.FindStatusCondition(got.Status.Conditions, v1.ConditionDegraded).LastTransitionTime.Time).To(BeTemporally("==", now.Time))

	// Worker-owned condition types are refused.
	g.Expect(e.ExecuteCluster(ctx, command.PatchSmbMountRemediationMirror{SmbMount: name, Conditions: []metav1.Condition{
		{Type: v1.ConditionHealthy, Status: metav1.ConditionFalse, Reason: "X", Message: "x", LastTransitionTime: now},
	}})).NotTo(Succeed())
	// A vanished SmbMount is not an error.
	g.Expect(e.ExecuteCluster(ctx, command.PatchSmbMountRemediationMirror{SmbMount: "does-not-exist",
		Remediation: &v1.ActiveRemediation{Active: false}})).To(Succeed())
}

func managedStatusFields(entries []metav1.ManagedFieldsEntry, manager string) string {
	var b strings.Builder
	for _, mf := range entries {
		if mf.Manager == manager && mf.Subresource == "status" && mf.FieldsV1 != nil {
			b.Write(mf.FieldsV1.GetRawBytes())
		}
	}
	return b.String()
}
