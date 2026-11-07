package v1alpha1

// Health is the debounced health of one mount (globalmount or pod mount).
// +kubebuilder:validation:Enum=Healthy;Suspect;Broken;Errored;Unknown
type Health string

const (
	HealthHealthy Health = "Healthy"
	HealthSuspect Health = "Suspect"
	HealthBroken  Health = "Broken"
	// HealthErrored means the probe itself failed (probe_error); never remediated.
	HealthErrored Health = "Errored"
	HealthUnknown Health = "Unknown"
)

// FailureReason classifies a failed probe.
// +kubebuilder:validation:Enum=not_mountpoint;wrong_fstype;stat_error;stat_timeout;probe_hung;probe_error
type FailureReason string

const (
	// ReasonNotMountpoint: the path is not a mountpoint in mountinfo (fast, structural).
	ReasonNotMountpoint FailureReason = "not_mountpoint"
	// ReasonWrongFSType: mountpoint with an fstype outside EXPECTED_FSTYPES, e.g. an xfs bind mount (fast, structural).
	ReasonWrongFSType FailureReason = "wrong_fstype"
	// ReasonStatError: stat/statfs returned an error (fast).
	ReasonStatError FailureReason = "stat_error"
	// ReasonStatTimeout: stat did not return within STAT_TIMEOUT (slow).
	ReasonStatTimeout FailureReason = "stat_timeout"
	// ReasonProbeHung: a previous probe child for this path is stuck (D-state) (slow).
	ReasonProbeHung FailureReason = "probe_hung"
	// ReasonProbeError: the probe machinery failed (spawn error etc.); never remediated.
	ReasonProbeError FailureReason = "probe_error"
)

// TriggerReason says why a remediation was created.
// +kubebuilder:validation:Enum=not_mountpoint;wrong_fstype;stat_error;stat_timeout;probe_hung;orphan_globalmount_broken;globalmount_not_empty;orphan_pod_dir_not_empty;orphan_pod_dir_unhealthy
type TriggerReason string

const (
	TriggerNotMountpoint TriggerReason = "not_mountpoint"
	TriggerWrongFSType   TriggerReason = "wrong_fstype"
	TriggerStatError     TriggerReason = "stat_error"
	TriggerStatTimeout   TriggerReason = "stat_timeout"
	TriggerProbeHung     TriggerReason = "probe_hung"
	// TriggerOrphanGlobalmountBroken: globalmount mounted and Broken, no pod references it.
	TriggerOrphanGlobalmountBroken TriggerReason = "orphan_globalmount_broken"
	// TriggerGlobalmountNotEmpty: globalmount not mounted but its directory is not empty.
	TriggerGlobalmountNotEmpty TriggerReason = "globalmount_not_empty"
	// TriggerOrphanPodDirNotEmpty: an orphan pod volume dir is not empty.
	TriggerOrphanPodDirNotEmpty TriggerReason = "orphan_pod_dir_not_empty"
	// TriggerOrphanPodDirUnhealthy: an orphan pod volume dir is mounted and unhealthy.
	TriggerOrphanPodDirUnhealthy TriggerReason = "orphan_pod_dir_unhealthy"
)

// RemediationType selects the remediation state machine.
// +kubebuilder:validation:Enum=Workload;Sweep
type RemediationType string

const (
	RemediationTypeWorkload RemediationType = "Workload"
	RemediationTypeSweep    RemediationType = "Sweep"
)

// Stage is a remediation state-machine stage.
// +kubebuilder:validation:Enum=pending;planning;draining;releasing_mounts;awaiting_unstage;quarantining;awaiting_unstage_completion;restarting_driver;scaling_up;verifying;sweeping;rolling_back;finalizing;done
type Stage string

const (
	StagePending                   Stage = "pending"
	StagePlanning                  Stage = "planning"
	StageDraining                  Stage = "draining"
	StageReleasingMounts           Stage = "releasing_mounts"
	StageAwaitingUnstage           Stage = "awaiting_unstage"
	StageQuarantining              Stage = "quarantining"
	StageAwaitingUnstageCompletion Stage = "awaiting_unstage_completion"
	StageRestartingDriver          Stage = "restarting_driver"
	StageScalingUp                 Stage = "scaling_up"
	StageVerifying                 Stage = "verifying"
	// StageSweeping is only used by Sweep remediations.
	StageSweeping    Stage = "sweeping"
	StageRollingBack Stage = "rolling_back"
	StageFinalizing  Stage = "finalizing"
	// StageDone is the only terminal stage; Outcome is set when it is entered.
	StageDone Stage = "done"
)

// Terminal reports whether s is the terminal stage.
func (s Stage) Terminal() bool { return s == StageDone }

// Outcome is the terminal result of a remediation.
// +kubebuilder:validation:Enum=healed;verify_failed;skipped_no_owner;rolled_back;dry_run;failed
type Outcome string

const (
	OutcomeHealed         Outcome = "healed"
	OutcomeVerifyFailed   Outcome = "verify_failed"
	OutcomeSkippedNoOwner Outcome = "skipped_no_owner"
	OutcomeRolledBack     Outcome = "rolled_back"
	OutcomeDryRun         Outcome = "dry_run"
	// OutcomeFailed: a host task failed or an unrecoverable error ended the episode.
	OutcomeFailed Outcome = "failed"
)

// WorkloadState is the per-workload progress inside a remediation scope.
// +kubebuilder:validation:Enum=in_scope;scaled_down;restored;deleted
type WorkloadState string

const (
	WorkloadInScope    WorkloadState = "in_scope"
	WorkloadScaledDown WorkloadState = "scaled_down"
	WorkloadRestored   WorkloadState = "restored"
	WorkloadDeleted    WorkloadState = "deleted"
)

// VerificationResult is the per-workload verification result.
// +kubebuilder:validation:Enum=pending;verified;elsewhere;failed
type VerificationResult string

const (
	VerificationPending   VerificationResult = "pending"
	VerificationVerified  VerificationResult = "verified"
	VerificationElsewhere VerificationResult = "elsewhere"
	VerificationFailed    VerificationResult = "failed"
)

// HostActionType is an action a worker performs on its node.
// +kubebuilder:validation:Enum=ForceUnmount;Quarantine;ProbeNow
type HostActionType string

const (
	HostActionForceUnmount HostActionType = "ForceUnmount"
	HostActionQuarantine   HostActionType = "Quarantine"
	HostActionProbeNow     HostActionType = "ProbeNow"
)

// OnlyIf is a precondition the worker re-checks against live mountinfo
// immediately before acting. Empty means "always".
// +kubebuilder:validation:Enum=NotHealthyMount;NotMountpoint
type OnlyIf string

const (
	// OnlyIfNotHealthyMount: act unless the path is a mountpoint with an expected fstype that probes healthy.
	OnlyIfNotHealthyMount OnlyIf = "NotHealthyMount"
	// OnlyIfNotMountpoint: act only if the path is not a mountpoint.
	OnlyIfNotMountpoint OnlyIf = "NotMountpoint"
)

// HostTaskPhase is the overall phase of an SmbHostTask.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type HostTaskPhase string

const (
	HostTaskPending   HostTaskPhase = "Pending"
	HostTaskRunning   HostTaskPhase = "Running"
	HostTaskSucceeded HostTaskPhase = "Succeeded"
	HostTaskFailed    HostTaskPhase = "Failed"
)

// HostActionOutcome is the result of one host action.
// +kubebuilder:validation:Enum=InProgress;Succeeded;Skipped;DryRun;Failed
type HostActionOutcome string

const (
	// HostActionInProgress is written before a Quarantine moves data
	// (write-ahead): it carries the planned destination, so a worker that
	// crashes mid-action still knows where the data went and where it came
	// from. It is not a final result: the action is re-run.
	HostActionInProgress HostActionOutcome = "InProgress"
	HostActionSucceeded  HostActionOutcome = "Succeeded"
	// HostActionSkipped: the OnlyIf precondition was false.
	HostActionSkipped HostActionOutcome = "Skipped"
	// HostActionDryRun: the task had dryRun=true; nothing was changed.
	HostActionDryRun HostActionOutcome = "DryRun"
	HostActionFailed HostActionOutcome = "Failed"
)

// MountKind distinguishes a volume's staging mount from a pod's bind mount
// (metric label "kind").
// +kubebuilder:validation:Enum=globalmount;pod
type MountKind string

const (
	MountKindGlobalmount MountKind = "globalmount"
	MountKindPod         MountKind = "pod"
)

// SuppressionReason is why the control plane did not start a remediation.
// Used as the RemediationSuppressed condition reason and metric label.
// +kubebuilder:validation:Enum=backoff;scope_conflict;active_remediation
type SuppressionReason string

const (
	SuppressionBackoff           SuppressionReason = "backoff"
	SuppressionScopeConflict     SuppressionReason = "scope_conflict"
	SuppressionActiveRemediation SuppressionReason = "active_remediation"
)

// WorkloadKind is a scalable workload kind the healer may touch.
// +kubebuilder:validation:Enum=Deployment;StatefulSet
type WorkloadKind string

const (
	WorkloadKindDeployment  WorkloadKind = "Deployment"
	WorkloadKindStatefulSet WorkloadKind = "StatefulSet"
)
