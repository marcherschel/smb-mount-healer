package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SmbQuarantineSpec is immutable after creation. Every path in it is the
// healer's own (quarantine folder, source mount path), never user data.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type SmbQuarantineSpec struct {
	// +kubebuilder:validation:MinLength=1
	NodeName string `json:"nodeName"`
	// Path is the absolute host path of the quarantine folder.
	// +kubebuilder:validation:Pattern=`^/`
	Path string `json:"path"`
	// +kubebuilder:validation:MinLength=1
	PVName string `json:"pvName"`
	// +optional
	Share string `json:"share,omitempty"`
	// SourcePath is the globalmount or pod directory the data was moved out of
	// (empty for adopted folders).
	// +optional
	SourcePath string `json:"sourcePath,omitempty"`
	// QuarantinedTime is when the folder was created (parsed from its name for adopted folders).
	QuarantinedTime metav1.Time `json:"quarantinedTime"`
	// RemediationRef names the SmbMountRemediation that quarantined the data; empty for adopted folders.
	// +optional
	RemediationRef string `json:"remediationRef,omitempty"`
}

// SmbQuarantineStatus holds aggregates only. It deliberately has no field
// that could carry a file or directory name from the quarantined data
// (enforced by privacy_test.go). Condition messages must be fixed templates.
type SmbQuarantineStatus struct {
	// Entries is the number of top-level entries in the folder.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Entries int64 `json:"entries,omitempty"`
	// Files is the number of regular files found by the (bounded) walk.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Files int64 `json:"files,omitempty"`
	// Bytes is the total size of regular files found by the (bounded) walk.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Bytes int64 `json:"bytes,omitempty"`
	// SizeTruncated is true when the walk budget (entries or time) ran out.
	// +optional
	SizeTruncated bool `json:"sizeTruncated,omitempty"`
	// +optional
	OldestModTime *metav1.Time `json:"oldestModTime,omitempty"`
	// +optional
	NewestModTime *metav1.Time `json:"newestModTime,omitempty"`
	// +optional
	LastScanTime *metav1.Time `json:"lastScanTime,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SmbQuarantine shows that quarantined shadow data exists in one folder on a
// node. It reflects the disk: it disappears when the folder is gone or empty,
// and deleting it does not acknowledge anything (it is recreated while data remains).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=smbq,categories=smbhealer
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="PV",type=string,JSONPath=`.spec.pvName`
// +kubebuilder:printcolumn:name="Files",type=integer,JSONPath=`.status.files`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.status.bytes`,description="Bytes"
// +kubebuilder:printcolumn:name="Quarantined",type=date,JSONPath=`.spec.quarantinedTime`
// +kubebuilder:printcolumn:name="Last Scan",type=date,JSONPath=`.status.lastScanTime`
type SmbQuarantine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SmbQuarantineSpec `json:"spec"`
	// +optional
	Status SmbQuarantineStatus `json:"status,omitempty"`
}

// SmbQuarantineList contains a list of SmbQuarantine.
//
// +kubebuilder:object:root=true
type SmbQuarantineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SmbQuarantine `json:"items"`
}

func init() {
	register(&SmbQuarantine{}, &SmbQuarantineList{})
}
