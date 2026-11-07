package v1alpha1

import "k8s.io/apimachinery/pkg/types"

// WorkloadRef identifies a scalable workload.
type WorkloadRef struct {
	Kind WorkloadKind `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// String returns "Kind/namespace/name".
func (w WorkloadRef) String() string {
	return string(w.Kind) + "/" + w.Namespace + "/" + w.Name
}

// PodRef identifies one pod instance.
type PodRef struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string    `json:"name"`
	UID  types.UID `json:"uid"`
}
