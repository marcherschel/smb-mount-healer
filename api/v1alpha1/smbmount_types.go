package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SmbMountSpec identifies one SMB-backed PV on one node. Set once by the
// worker at creation and never changed afterwards.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SmbMountSpec struct {
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`
	// +kubebuilder:validation:MinLength=1
	PVName string `json:"pvName"`
	// Share is the SMB source of the PV (volumeAttributes.source), e.g. //host/share.
	// +kubebuilder:validation:MinLength=1
	Share string `json:"share"`
	// +kubebuilder:validation:MinLength=1
	VolumeHandle string `json:"volumeHandle"`
	// GlobalmountPath is the CSI staging path: <kubelet>/plugins/kubernetes.io/csi/<driver>/<sha256(volumeHandle)>/globalmount.
	// +kubebuilder:validation:Pattern=`^/`
	GlobalmountPath string `json:"globalmountPath"`
}

// Debounce is the persisted progress of the per-mount health FSM, so a worker
// restart neither resets nor re-triggers anything.
type Debounce struct {
	// +optional
	FirstFailureTime *metav1.Time `json:"firstFailureTime,omitempty"`
	// +kubebuilder:validation:Minimum=0
	ConsecutiveFailures int32 `json:"consecutiveFailures"`
	// +optional
	LastClass FailureReason `json:"lastClass,omitempty"`
}

// GlobalmountStatus is the live state of the staging mount.
type GlobalmountStatus struct {
	Mounted bool `json:"mounted"`
	// +optional
	FSType string `json:"fstype,omitempty"`
	// +optional
	SuperOptions string `json:"superOptions,omitempty"`
	// DirExists reports whether the globalmount directory exists.
	DirExists bool `json:"dirExists"`
	// Empty is only meaningful when the directory exists and is not mounted.
	Empty bool `json:"empty"`
}

// PodMountStatus is one pod's bind mount of this PV.
type PodMountStatus struct {
	Namespace string    `json:"namespace"`
	Pod       string    `json:"pod"`
	UID       types.UID `json:"uid"`
	// Workload is a display hint "Kind/name" derived by the worker from the pod's
	// owner (ReplicaSet names are mapped to their Deployment). The control plane
	// resolves owners authoritatively.
	// +optional
	Workload string `json:"workload,omitempty"`
	Mounted  bool   `json:"mounted"`
	// +optional
	FSType string `json:"fstype,omitempty"`
	// +optional
	Health Health `json:"health,omitempty"`
	// +optional
	Reason FailureReason `json:"reason,omitempty"`
}

// OrphanPodDir is a pod volume directory for this PV whose pod UID no longer
// exists on the node.
type OrphanPodDir struct {
	UID types.UID `json:"uid"`
	// +kubebuilder:validation:Pattern=`^/`
	Path    string `json:"path"`
	Mounted bool   `json:"mounted"`
	Empty   bool   `json:"empty"`
}

// ActiveRemediation mirrors the remediation currently touching this mount.
type ActiveRemediation struct {
	Active bool `json:"active"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Stage Stage `json:"stage,omitempty"`
	// +optional
	StageEnteredTime *metav1.Time `json:"stageEnteredTime,omitempty"`
}

// LastRemediation mirrors the last finished remediation touching this mount.
type LastRemediation struct {
	Name string `json:"name"`
	// +optional
	Outcome Outcome `json:"outcome,omitempty"`
	// +optional
	StartedTime *metav1.Time `json:"startedTime,omitempty"`
	// +optional
	FinishedTime *metav1.Time `json:"finishedTime,omitempty"`
}

// SmbMountStatus is split between two writers via server-side-apply field
// managers. Every top-level field is optional so that one manager's apply
// never claims the other manager's fields.
type SmbMountStatus struct {
	// --- worker (FieldManagerWorker) ---

	// +optional
	Health Health `json:"health,omitempty"`
	// +optional
	Reason FailureReason `json:"reason,omitempty"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +optional
	Debounce *Debounce `json:"debounce,omitempty"`
	// +optional
	Globalmount *GlobalmountStatus `json:"globalmount,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=uid
	PodMounts []PodMountStatus `json:"podMounts,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=path
	OrphanPodDirs []OrphanPodDir `json:"orphanPodDirs,omitempty"`
	// PendingQuarantines is the number of SmbQuarantine objects for this node and PV.
	// +optional
	// +kubebuilder:validation:Minimum=0
	PendingQuarantines int32 `json:"pendingQuarantines,omitempty"`

	// --- control plane (FieldManagerControlPlane) ---

	// +optional
	Remediation *ActiveRemediation `json:"remediation,omitempty"`
	// +optional
	LastRemediation *LastRemediation `json:"lastRemediation,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=0
	RemediationsLastHour int32 `json:"remediationsLastHour,omitempty"`

	// --- shared, per condition type ---
	// Healthy, ShadowDataPending: worker. Remediating, Degraded, RemediationSuppressed: control plane.

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SmbMount reports the health of one SMB PV on one node.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=smbm,categories=smbhealer
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="PV",type=string,JSONPath=`.spec.pvName`
// +kubebuilder:printcolumn:name="Health",type=string,JSONPath=`.status.health`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.remediation.stage`
// +kubebuilder:printcolumn:name="Last Outcome",type=string,JSONPath=`.status.lastRemediation.outcome`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SmbMount struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SmbMountSpec `json:"spec"`
	// +optional
	Status SmbMountStatus `json:"status,omitempty"`
}

// SmbMountList contains a list of SmbMount.
//
// +kubebuilder:object:root=true
type SmbMountList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmbMount `json:"items"`
}

func init() {
	register(&SmbMount{}, &SmbMountList{})
}
