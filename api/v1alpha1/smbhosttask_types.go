package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostAction is one idempotent action for the worker on spec.nodeName.
type HostAction struct {
	// ID is unique within the task; a result with this ID means "already done".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ID   string         `json:"id"`
	Type HostActionType `json:"type"`
	// Path is an absolute host path under the kubelet root.
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`
	// PV is required for Quarantine (it names the quarantine folder).
	// +optional
	PV string `json:"pv,omitempty"`
	// OnlyIf is re-checked against live mountinfo right before acting.
	// +optional
	OnlyIf OnlyIf `json:"onlyIf,omitempty"`
}

// SmbHostTaskSpec is immutable after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SmbHostTaskSpec struct {
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`
	// DryRun makes the worker only log what it would do.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=id
	Actions []HostAction `json:"actions"`
}

// HostActionResult is the worker's result for one action.
type HostActionResult struct {
	// +kubebuilder:validation:MinLength=1
	ID     string            `json:"id"`
	Result HostActionOutcome `json:"result"`
	// Destination is the quarantine folder (the healer's own path) for Quarantine actions.
	// +optional
	Destination string `json:"destination,omitempty"`
	// Entries is the number of top-level entries moved by Quarantine.
	// +optional
	Entries int32 `json:"entries,omitempty"`
	// Error is a short error text. It MUST NOT contain entry names from user data.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Error string      `json:"error,omitempty"`
	Time  metav1.Time `json:"time"`
}

// SmbHostTaskStatus is written by the worker only.
type SmbHostTaskStatus struct {
	// +optional
	Phase HostTaskPhase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=id
	Results []HostActionResult `json:"results,omitempty"`
}

// SmbHostTask tells one worker which host actions to run.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=smbt,categories=smbhealer
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Dry-Run",type=boolean,JSONPath=`.spec.dryRun`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SmbHostTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SmbHostTaskSpec `json:"spec"`
	// +optional
	Status SmbHostTaskStatus `json:"status,omitempty"`
}

// SmbHostTaskList contains a list of SmbHostTask.
//
// +kubebuilder:object:root=true
type SmbHostTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmbHostTask `json:"items"`
}

func init() {
	register(&SmbHostTask{}, &SmbHostTaskList{})
}
