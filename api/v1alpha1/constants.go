package v1alpha1

// Finalizers, annotations and labels.
const (
	// FinalizerRestoreReplicas is set on every SmbMountRemediation. If the
	// object is deleted before reaching a terminal stage, the control plane
	// scales every in-scope workload back to its originalReplicas before
	// removing the finalizer. Deleting a remediation is the rollback mechanism.
	FinalizerRestoreReplicas = Group + "/restore-replicas"

	// AnnotationAbort, set to "true" on an SmbMountRemediation, sends a
	// non-terminal remediation to the rolling_back stage.
	AnnotationAbort = Group + "/abort"

	// LabelNode carries the node name on SmbMount, SmbHostTask,
	// SmbMountRemediation and SmbQuarantine objects (used for per-node watches).
	LabelNode = Group + "/node"

	// LabelPV carries the PV name on SmbMount and SmbQuarantine objects.
	LabelPV = Group + "/pv"

	// LabelRemediation carries the owning SmbMountRemediation name on SmbHostTasks.
	LabelRemediation = Group + "/remediation"
)

// Server-side-apply field managers. Each status field group has exactly one writer.
const (
	FieldManagerWorker       = "smb-mount-healer-worker"
	FieldManagerControlPlane = "smb-mount-healer-controlplane"
)

// Condition types on SmbMount.
const (
	// ConditionHealthy is written by the worker.
	ConditionHealthy = "Healthy"
	// ConditionShadowDataPending is written by the worker (pendingQuarantines > 0).
	ConditionShadowDataPending = "ShadowDataPending"
	// ConditionRemediating is written by the control plane.
	ConditionRemediating = "Remediating"
	// ConditionDegraded is written by the control plane (last outcome not healed).
	ConditionDegraded = "Degraded"
	// ConditionRemediationSuppressed is written by the control plane when a
	// trigger was suppressed (backoff, scope conflict); reason = SuppressionReason.
	ConditionRemediationSuppressed = "RemediationSuppressed"
)

// Condition types on SmbMountRemediation.
const (
	ConditionAborted       = "Aborted"
	ConditionScopeConflict = "ScopeConflict"
)

// Condition types on SmbQuarantine.
const (
	// ConditionPending is True while the quarantine folder still holds data.
	// Its message must be a fixed template: never include entry names.
	ConditionPending = "Pending"
)

// MaxTransitions bounds SmbMountRemediationStatus.Transitions (oldest dropped first).
const MaxTransitions = 50
