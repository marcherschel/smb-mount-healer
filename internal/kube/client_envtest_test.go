//go:build envtest

package kube_test

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/marcherschel/smb-mount-healer/api/v1alpha1"
	"github.com/marcherschel/smb-mount-healer/internal/kube"
)

var (
	restCfg *rest.Config
	scheme  *runtime.Scheme
	k8s     client.Client
)

func TestMain(m *testing.M) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		panic("starting envtest (run via `make test-envtest`): " + err.Error())
	}
	restCfg = cfg
	if scheme, err = kube.NewScheme(); err != nil {
		panic(err)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
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
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kube-test-"}}
	if err := k8s.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	return ctx, ns.Name
}

func newCluster(t *testing.T, c client.Client, rec events.EventRecorder, driverNS string) *kube.ClientCluster {
	t.Helper()
	kc, err := kube.NewClientCluster(kube.ClientOptions{
		Client: c, Recorder: rec, DriverNamespace: driverNS,
		DriverSelector: labels.SelectorFromSet(labels.Set{"app": "csi-smb-node"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return kc
}

func podSpec(node string) corev1.PodSpec {
	return corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}
}

func selector(app string) (*metav1.LabelSelector, map[string]string) {
	l := map[string]string{"app": app}
	return &metav1.LabelSelector{MatchLabels: l}, l
}

func deployment(ns, name string, replicas int32) *appsv1.Deployment {
	sel, l := selector(name)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Selector: sel,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l}, Spec: podSpec("")}},
	}
}

func statefulSet(ns, name string, replicas int32) *appsv1.StatefulSet {
	sel, l := selector(name)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas), Selector: sel,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l}, Spec: podSpec("")}},
	}
}

func controllerRef(obj client.Object, gvk string) metav1.OwnerReference {
	kinds := map[string]string{"Deployment": "apps/v1", "ReplicaSet": "apps/v1", "StatefulSet": "apps/v1", "DaemonSet": "apps/v1"}
	return metav1.OwnerReference{APIVersion: kinds[gvk], Kind: gvk, Name: obj.GetName(), UID: obj.GetUID(), Controller: ptr.To(true)}
}

func TestScaleSubresource(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	kc := newCluster(t, k8s, nil, "kube-system")

	d := deployment(ns, "web", 2)
	s := statefulSet(ns, "db", 3)
	g.Expect(k8s.Create(ctx, d)).To(Succeed())
	g.Expect(k8s.Create(ctx, s)).To(Succeed())

	for _, tc := range []struct {
		ref  v1.WorkloadRef
		orig int32
	}{
		{v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: ns, Name: "web"}, 2},
		{v1.WorkloadRef{Kind: v1.WorkloadKindStatefulSet, Namespace: ns, Name: "db"}, 3},
	} {
		st, err := kc.Workload(ctx, tc.ref)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(st).To(Equal(kube.WorkloadState{Exists: true, Replicas: tc.orig}))

		changed, err := kc.ScaleReport(ctx, tc.ref, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeTrue(), tc.ref.String())
		changed, err = kc.ScaleReport(ctx, tc.ref, 0)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(changed).To(BeFalse(), "second scale to the same count must be a no-op")

		st, err = kc.Workload(ctx, tc.ref)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(st.Replicas).To(BeZero())

		g.Expect(kc.Scale(ctx, tc.ref, tc.orig)).To(Succeed())
		st, err = kc.Workload(ctx, tc.ref)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(st.Replicas).To(Equal(tc.orig))
	}

	// The object's spec.replicas reflects the scale subresource writes.
	var got appsv1.StatefulSet
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(s), &got)).To(Succeed())
	g.Expect(*got.Spec.Replicas).To(Equal(int32(3)))

	missing := v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: ns, Name: "gone"}
	st, err := kc.Workload(ctx, missing)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(st.Exists).To(BeFalse())
	err = kc.Scale(ctx, missing, 1)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "scaling a missing workload is NotFound: %v", err)
	g.Expect(kc.Scale(ctx, v1.WorkloadRef{Kind: "DaemonSet", Namespace: ns, Name: "x"}, 1)).NotTo(Succeed())
}

func TestResolveOwner(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	kc := newCluster(t, k8s, nil, "kube-system")

	d := deployment(ns, "web", 1)
	g.Expect(k8s.Create(ctx, d)).To(Succeed())
	s := statefulSet(ns, "db", 1)
	g.Expect(k8s.Create(ctx, s)).To(Succeed())

	rsSpec := func(app string) appsv1.ReplicaSetSpec {
		sel, l := selector(app)
		return appsv1.ReplicaSetSpec{Selector: sel, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: l}, Spec: podSpec("")}}
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web-abc",
		OwnerReferences: []metav1.OwnerReference{controllerRef(d, "Deployment")}}, Spec: rsSpec("web")}
	g.Expect(k8s.Create(ctx, rs)).To(Succeed())
	bareRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "bare"}, Spec: rsSpec("bare")}
	g.Expect(k8s.Create(ctx, bareRS)).To(Succeed())
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ds"}}
	ds.Spec.Selector, ds.Spec.Template.Labels = selector("ds")
	ds.Spec.Template.Spec = podSpec("")
	g.Expect(k8s.Create(ctx, ds)).To(Succeed())
	goneRS := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "gone", UID: "00000000-0000-0000-0000-000000000001"}}

	tests := []struct {
		name  string
		owner []metav1.OwnerReference
		want  *v1.WorkloadRef
	}{
		{"deployment", []metav1.OwnerReference{controllerRef(rs, "ReplicaSet")}, &v1.WorkloadRef{Kind: v1.WorkloadKindDeployment, Namespace: ns, Name: "web"}},
		{"statefulset", []metav1.OwnerReference{controllerRef(s, "StatefulSet")}, &v1.WorkloadRef{Kind: v1.WorkloadKindStatefulSet, Namespace: ns, Name: "db"}},
		{"bare-rs", []metav1.OwnerReference{controllerRef(bareRS, "ReplicaSet")}, nil},
		{"missing-rs", []metav1.OwnerReference{controllerRef(goneRS, "ReplicaSet")}, nil},
		{"daemonset", []metav1.OwnerReference{controllerRef(ds, "DaemonSet")}, nil},
		{"bare-pod", nil, nil},
	}
	for _, tt := range tests {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: tt.name, OwnerReferences: tt.owner}, Spec: podSpec("")}
		g.Expect(k8s.Create(ctx, pod)).To(Succeed(), tt.name)
		got, err := kc.ResolveOwner(ctx, pod)
		g.Expect(err).NotTo(HaveOccurred(), tt.name)
		g.Expect(got).To(Equal(tt.want), tt.name)
	}
}

func TestDeletePodUIDPrecondition(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	kc := newCluster(t, k8s, nil, "kube-system")

	// Unscheduled pods are deleted immediately (no kubelet in envtest).
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "driver"}, Spec: podSpec("")}
	g.Expect(k8s.Create(ctx, pod)).To(Succeed())

	deleted, err := kc.DeletePodReport(ctx, ns, "driver", "11111111-2222-3333-4444-555555555555")
	g.Expect(err).NotTo(HaveOccurred(), "UID mismatch counts as already done")
	g.Expect(deleted).To(BeFalse())
	g.Expect(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})).To(Succeed(), "pod with another UID must survive")

	g.Expect(kc.DeletePod(ctx, ns, "driver", "")).NotTo(Succeed(), "an empty UID is refused")

	deleted, err = kc.DeletePodReport(ctx, ns, "driver", pod.UID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(deleted).To(BeTrue())
	g.Expect(apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}))).To(BeTrue())

	deleted, err = kc.DeletePodReport(ctx, ns, "driver", pod.UID)
	g.Expect(err).NotTo(HaveOccurred(), "NotFound counts as already done")
	g.Expect(deleted).To(BeFalse())
}

func TestDriverPods(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	other := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "kube-test-other-"}}
	g.Expect(k8s.Create(ctx, other)).To(Succeed())
	kc := newCluster(t, k8s, nil, ns)

	mk := func(ns, name, node, app string) {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: map[string]string{"app": app}}, Spec: podSpec(node)}
		g.Expect(k8s.Create(ctx, p)).To(Succeed())
	}
	mk(ns, "driver-a", "node-a", "csi-smb-node")
	mk(ns, "driver-b", "node-b", "csi-smb-node")
	mk(ns, "controller-a", "node-a", "csi-smb-controller")
	mk(other.Name, "driver-a", "node-a", "csi-smb-node")

	pods, err := kc.DriverPods(ctx, "node-a")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(HaveLen(1))
	g.Expect(pods[0].Namespace).To(Equal(ns))
	g.Expect(pods[0].Name).To(Equal("driver-a"))

	pods, err = kc.DriverPods(ctx, "node-c")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(BeEmpty())
}

func TestEventRecorder(t *testing.T) {
	g := NewWithT(t)
	ctx, ns := setup(t)
	cs, err := kubernetes.NewForConfig(restCfg)
	g.Expect(err).NotTo(HaveOccurred())
	b := events.NewBroadcaster(&events.EventSinkImpl{Interface: cs.EventsV1()})
	g.Expect(b.StartRecordingToSinkWithContext(ctx)).To(Succeed())
	t.Cleanup(b.Shutdown)
	kc := newCluster(t, k8s, b.NewRecorder(scheme, v1.FieldManagerControlPlane), ns)

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "app"}, Spec: podSpec("")}
	g.Expect(k8s.Create(ctx, pod)).To(Succeed())
	kc.Event(pod, corev1.EventTypeWarning, "StillStaged", "", "100% of pods still keep the volume staged")

	g.Eventually(func(g Gomega) {
		var list eventsv1.EventList
		g.Expect(k8s.List(ctx, &list, client.InNamespace(ns))).To(Succeed())
		g.Expect(list.Items).To(HaveLen(1))
		ev := list.Items[0]
		g.Expect(ev.Regarding.Kind).To(Equal("Pod"))
		g.Expect(ev.Regarding.UID).To(Equal(pod.UID))
		g.Expect(ev.Reason).To(Equal("StillStaged"))
		g.Expect(ev.Action).To(Equal("StillStaged"), "empty action defaults to reason")
		g.Expect(ev.Note).To(Equal("100% of pods still keep the volume staged"))
		g.Expect(ev.Type).To(Equal(corev1.EventTypeWarning))
	}).WithTimeout(30 * time.Second).Should(Succeed())
}
