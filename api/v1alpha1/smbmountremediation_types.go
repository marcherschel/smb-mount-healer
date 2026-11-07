package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// RemediationTrigger records what caused the remediation.
type RemediationTrigger struct {
	// SmbMounts are the names of the SmbMount objects that triggered the episode.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	SmbMounts []string `json:"smbMounts"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Reasons []TriggerReason `json:"reasons,omitempty"`
}

// SmbMountRemediationSpec is immutable after creation.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SmbMountRemediationSpec struct {
	// +kubebuilder:validation:MinLength=1
	NodeName string             `json:"nodeName"`
	Type     RemediationType    `json:"type"`
	Trigger  RemediationTrigger `json:"trigger"`
	// DryRun walks every stage without scaling, deleting pods or touching hosts.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

// ScopedWorkload is one workload the remediation scales.
type ScopedWorkload struct {
	WorkloadRef `json:",inline"`
	// OriginalReplicas is written before the first scale-down and restored on scale-up/rollback.
	// +kubebuilder:validation:Minimum=0
	OriginalReplicas int32 `json:"originalReplicas"`
	// +optional
	State WorkloadState `json:"state,omitempty"`
}

// RemediationScope is everything an episode may touch.
type RemediationScope struct {
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Workloads []ScopedWorkload `json:"workloads,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=set
	PVs []string `json:"pvs,omitempty"`
	// DrainPods are the pod instances that must be gone before releasing mounts.
	// +optional
	// +kubebuilder:validation:MaxItems=512
	DrainPods []PodRef `json:"drainPods,omitempty"`
}

// DeadGlobalmount is a globalmount found dead during the episode.
type DeadGlobalmount struct {
	// +kubebuilder:validation:MinLength=1
	PVName string `json:"pvName"`
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`
}

// DriverRestart records the CSI node driver pod restart.
type DriverRestart struct {
	// +optional
	OldPodNamespace string `json:"oldPodNamespace,omitempty"`
	// +optional
	OldPodName string `json:"oldPodName,omitempty"`
	// +optional
	OldPodUID types.UID `json:"oldPodUID,omitempty"`
	// +optional
	NewPodUID types.UID `json:"newPodUID,omitempty"`
}

// HostTaskRef tracks an SmbHostTask created by this remediation.
type HostTaskRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Stage is the stage that created the task.
	// +optional
	Stage Stage `json:"stage,omitempty"`
	// +optional
	Phase HostTaskPhase `json:"phase,omitempty"`
}

// WorkloadVerification is the post-scale-up verification of one workload.
type WorkloadVerification struct {
	Workload WorkloadRef        `json:"workload"`
	Result   VerificationResult `json:"result"`
}

// StageTransition is one state-machine transition.
type StageTransition struct {
	// +optional
	From Stage `json:"from,omitempty"`
	To   Stage `json:"to"`
	// +optional
	Reason string      `json:"reason,omitempty"`
	Time   metav1.Time `json:"time"`
}

// SmbMountRemediationStatus is the remediation state machine's state. The
// control plane writes it BEFORE executing the commands of a step (write-ahead).
type SmbMountRemediationStatus struct {
	// +optional
	Stage Stage `json:"stage,omitempty"`
	// +optional
	StageEnteredTime *metav1.Time `json:"stageEnteredTime,omitempty"`
	// +optional
	StartedTime *metav1.Time `json:"startedTime,omitempty"`
	// Deadline is the absolute deadline of the current stage's wait guard.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
	// +optional
	Scope *RemediationScope `json:"scope,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=128
	DeadGlobalmounts []DeadGlobalmount `json:"deadGlobalmounts,omitempty"`
	// +optional
	Driver *DriverRestart `json:"driver,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=64
	HostTasks []HostTaskRef `json:"hostTasks,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Verification []WorkloadVerification `json:"verification,omitempty"`
	// Outcome is set when the remediation reaches the terminal stage.
	// +optional
	Outcome Outcome `json:"outcome,omitempty"`
	// Message is a human-readable summary of the outcome.
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Transitions holds the last MaxTransitions (50) transitions, oldest first.
	// +optional
	// +kubebuilder:validation:MaxItems=50
	Transitions []StageTransition `json:"transitions,omitempty"`
}

// SmbMountRemediation is one remediation episode on one node.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=smbr,categories=smbhealer
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Stage",type=string,JSONPath=`.status.stage`
// +kubebuilder:printcolumn:name="Outcome",type=string,JSONPath=`.status.outcome`
// +kubebuilder:printcolumn:name="Dry-Run",type=boolean,JSONPath=`.spec.dryRun`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type SmbMountRemediation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SmbMountRemediationSpec `json:"spec"`
	// +optional
	Status SmbMountRemediationStatus `json:"status,omitempty"`
}

// SmbMountRemediationList contains a list of SmbMountRemediation.
//
// +kubebuilder:object:root=true
type SmbMountRemediationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmbMountRemediation `json:"items"`
}

func init() {
	register(&SmbMountRemediation{}, &SmbMountRemediationList{})
}
