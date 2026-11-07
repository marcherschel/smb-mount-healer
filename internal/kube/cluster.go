package kube

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// WorkloadState is the live state of a Deployment or StatefulSet.
type WorkloadState struct {
	Exists        bool
	Replicas      int32 // spec.replicas as reported by the scale subresource
	ReadyReplicas int32
}

// Cluster is everything the control plane does to non-healer objects.
// Implementations must be idempotent: NotFound on delete and UID-precondition
// conflicts are not errors.
type Cluster interface {
	// ResolveOwner returns the controlling Deployment or StatefulSet of pod,
	// or nil if it has none (bare pod, bare ReplicaSet, other kinds).
	ResolveOwner(ctx context.Context, pod *corev1.Pod) (*v1alpha1.WorkloadRef, error)
	// Workload reads the workload's scale; Exists=false if it is gone.
	Workload(ctx context.Context, ref v1alpha1.WorkloadRef) (WorkloadState, error)
	// Scale sets spec.replicas through the scale subresource.
	Scale(ctx context.Context, ref v1alpha1.WorkloadRef, replicas int32) error
	// DeletePod deletes namespace/name only if its UID still equals uid.
	DeletePod(ctx context.Context, namespace, name string, uid types.UID) error
	// DriverPods lists the CSI node driver pods (configured namespace and
	// label selector) scheduled on nodeName.
	DriverPods(ctx context.Context, nodeName string) ([]corev1.Pod, error)
	// Event records an events.k8s.io Event about regarding.
	Event(regarding runtime.Object, eventType, reason, action, message string)
}
