package kube

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// ChangeReporter is optionally implemented by a Cluster to tell the caller
// whether a mutating call actually changed anything. The cluster executor
// uses it so that workload_actions_total counts real actions only, not
// idempotent re-executions of the same command.
type ChangeReporter interface {
	// ScaleReport is Scale, returning changed=false if replicas already
	// matched. A missing workload is returned as a NotFound error.
	ScaleReport(ctx context.Context, ref v1alpha1.WorkloadRef, replicas int32) (changed bool, err error)
	// DeletePodReport is DeletePod, returning deleted=false if the pod was
	// already gone or its UID no longer matched.
	DeletePodReport(ctx context.Context, namespace, name string, uid types.UID) (deleted bool, err error)
}

// ClientOptions configures NewClientCluster.
type ClientOptions struct {
	// Client performs writes (scale subresource, pod deletion). Required.
	// In the control plane this is mgr.GetClient().
	Client client.Client
	// Reader reads pods (driver pods). Defaults to Client, which in the
	// control plane is backed by the manager's informer cache.
	Reader client.Reader
	// APIReader reads ReplicaSets and workloads live (uncached). Defaults
	// to Client. In the control plane this is mgr.GetAPIReader().
	APIReader client.Reader
	// Recorder records events.k8s.io Events, e.g.
	// mgr.GetEventRecorder("smb-mount-healer-controlplane"). Nil disables events.
	Recorder events.EventRecorder
	// DriverNamespace and DriverSelector locate the CSI node driver pods
	// (CSI_NODE_DRIVER_NAMESPACE, CSI_NODE_DRIVER_LABEL_SELECTOR).
	DriverNamespace string
	DriverSelector  labels.Selector
}

// ClientCluster implements Cluster (and ChangeReporter) over a controller-runtime client.
type ClientCluster struct {
	c         client.Client
	reader    client.Reader
	apiReader client.Reader
	recorder  events.EventRecorder
	driverNS  string
	driverSel labels.Selector
}

var (
	_ Cluster        = (*ClientCluster)(nil)
	_ ChangeReporter = (*ClientCluster)(nil)
)

// NewClientCluster returns a Cluster backed by controller-runtime clients.
func NewClientCluster(o ClientOptions) (*ClientCluster, error) {
	if o.Client == nil {
		return nil, errors.New("kube: ClientOptions.Client is required")
	}
	if o.DriverNamespace == "" {
		return nil, errors.New("kube: ClientOptions.DriverNamespace is required")
	}
	if o.DriverSelector == nil || o.DriverSelector.Empty() {
		return nil, errors.New("kube: ClientOptions.DriverSelector must not be empty")
	}
	cc := &ClientCluster{
		c: o.Client, reader: o.Reader, apiReader: o.APIReader, recorder: o.Recorder,
		driverNS: o.DriverNamespace, driverSel: o.DriverSelector,
	}
	if cc.reader == nil {
		cc.reader = o.Client
	}
	if cc.apiReader == nil {
		cc.apiReader = o.Client
	}
	return cc, nil
}

var replicaSetGVK = appsv1.SchemeGroupVersion.WithKind("ReplicaSet")

// ResolveOwner follows the pod's controller reference: Pod -> ReplicaSet ->
// Deployment and Pod -> StatefulSet. The ReplicaSet is read live (metadata
// only). It returns nil, nil for bare pods, bare ReplicaSets, ReplicaSets
// that no longer exist and any other controller kind (DaemonSet, Job, ...).
func (k *ClientCluster) ResolveOwner(ctx context.Context, pod *corev1.Pod) (*v1alpha1.WorkloadRef, error) {
	ctrl := metav1.GetControllerOf(pod)
	if ctrl == nil || !isApps(ctrl) {
		return nil, nil
	}
	switch v1alpha1.WorkloadKind(ctrl.Kind) {
	case v1alpha1.WorkloadKindDeployment, v1alpha1.WorkloadKindStatefulSet:
		return &v1alpha1.WorkloadRef{Kind: v1alpha1.WorkloadKind(ctrl.Kind), Namespace: pod.Namespace, Name: ctrl.Name}, nil
	}
	if ctrl.Kind != replicaSetGVK.Kind {
		return nil, nil
	}
	rs := &metav1.PartialObjectMetadata{}
	rs.SetGroupVersionKind(replicaSetGVK)
	if err := k.apiReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: ctrl.Name}, rs); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get ReplicaSet %s/%s: %w", pod.Namespace, ctrl.Name, err)
	}
	if ctrl.UID != "" && rs.UID != ctrl.UID {
		// The ReplicaSet was recreated under the same name; it does not own this pod.
		return nil, nil
	}
	rsCtrl := metav1.GetControllerOf(rs)
	if rsCtrl == nil || !isApps(rsCtrl) || rsCtrl.Kind != string(v1alpha1.WorkloadKindDeployment) {
		return nil, nil
	}
	return &v1alpha1.WorkloadRef{Kind: v1alpha1.WorkloadKindDeployment, Namespace: pod.Namespace, Name: rsCtrl.Name}, nil
}

func isApps(ref *metav1.OwnerReference) bool {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == appsv1.GroupName
}

// Workload reads the Deployment or StatefulSet live. Exists=false if it is gone.
func (k *ClientCluster) Workload(ctx context.Context, ref v1alpha1.WorkloadRef) (WorkloadState, error) {
	obj, err := workloadObject(ref)
	if err != nil {
		return WorkloadState{}, err
	}
	if err := k.apiReader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return WorkloadState{}, nil
		}
		return WorkloadState{}, fmt.Errorf("get %s: %w", ref, err)
	}
	var spec *int32
	var ready int32
	switch o := obj.(type) {
	case *appsv1.Deployment:
		spec, ready = o.Spec.Replicas, o.Status.ReadyReplicas
	case *appsv1.StatefulSet:
		spec, ready = o.Spec.Replicas, o.Status.ReadyReplicas
	}
	replicas := int32(1) // API default for a nil spec.replicas
	if spec != nil {
		replicas = *spec
	}
	return WorkloadState{Exists: true, Replicas: replicas, ReadyReplicas: ready}, nil
}

// Scale sets spec.replicas through the scale subresource; a no-op if it
// already matches. Conflicts are retried. A missing workload is a NotFound error.
func (k *ClientCluster) Scale(ctx context.Context, ref v1alpha1.WorkloadRef, replicas int32) error {
	_, err := k.ScaleReport(ctx, ref, replicas)
	return err
}

// ScaleReport implements ChangeReporter.
func (k *ClientCluster) ScaleReport(ctx context.Context, ref v1alpha1.WorkloadRef, replicas int32) (bool, error) {
	if replicas < 0 {
		return false, fmt.Errorf("scale %s: negative replicas %d", ref, replicas)
	}
	obj, err := workloadObject(ref)
	if err != nil {
		return false, err
	}
	obj.SetNamespace(ref.Namespace)
	obj.SetName(ref.Name)
	changed := false
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		sc := &autoscalingv1.Scale{}
		if err := k.c.SubResource("scale").Get(ctx, obj, sc); err != nil {
			return err
		}
		if sc.Spec.Replicas == replicas {
			return nil
		}
		sc.Spec.Replicas = replicas // sc carries resourceVersion: optimistic concurrency
		if err := k.c.SubResource("scale").Update(ctx, obj, client.WithSubResourceBody(sc)); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("scale %s to %d: %w", ref, replicas, err)
	}
	return changed, nil
}

func workloadObject(ref v1alpha1.WorkloadRef) (client.Object, error) {
	switch ref.Kind {
	case v1alpha1.WorkloadKindDeployment:
		return &appsv1.Deployment{}, nil
	case v1alpha1.WorkloadKindStatefulSet:
		return &appsv1.StatefulSet{}, nil
	}
	return nil, fmt.Errorf("unsupported workload kind %q", ref.Kind)
}

// DeletePod deletes namespace/name with a UID precondition. Already done
// counts as success: NotFound (the pod is gone) and a precondition conflict
// (a pod of that name exists but with a different UID, i.e. the original
// was already replaced) both return nil without deleting anything. An empty
// uid is rejected: this adapter never deletes a pod unconditionally.
func (k *ClientCluster) DeletePod(ctx context.Context, namespace, name string, uid types.UID) error {
	_, err := k.DeletePodReport(ctx, namespace, name, uid)
	return err
}

// DeletePodReport implements ChangeReporter.
func (k *ClientCluster) DeletePodReport(ctx context.Context, namespace, name string, uid types.UID) (bool, error) {
	if uid == "" {
		return false, fmt.Errorf("delete pod %s/%s: refusing to delete without a UID precondition", namespace, name)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	err := k.c.Delete(ctx, pod, client.Preconditions{UID: &uid})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return false, nil
	default:
		return false, fmt.Errorf("delete pod %s/%s (uid %s): %w", namespace, name, uid, err)
	}
}

// DriverPods lists the CSI node driver pods (configured namespace and label
// selector) whose spec.nodeName is nodeName. The node filter is applied
// client-side so no field index is needed on the cache.
func (k *ClientCluster) DriverPods(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	var list corev1.PodList
	if err := k.reader.List(ctx, &list, client.InNamespace(k.driverNS), client.MatchingLabelsSelector{Selector: k.driverSel}); err != nil {
		return nil, fmt.Errorf("list driver pods in %s (%s): %w", k.driverNS, k.driverSel, err)
	}
	out := make([]corev1.Pod, 0, 1)
	for i := range list.Items {
		if list.Items[i].Spec.NodeName == nodeName {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// Event records an events.k8s.io Event about regarding. An empty action
// defaults to reason (events.k8s.io requires one). The message is passed
// verbatim, never as a format string.
func (k *ClientCluster) Event(regarding runtime.Object, eventType, reason, action, message string) {
	if k.recorder == nil {
		return
	}
	if action == "" {
		action = reason
	}
	k.recorder.Eventf(regarding, nil, eventType, reason, action, "%s", message)
}
