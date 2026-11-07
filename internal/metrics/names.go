package metrics

// Namespace is the metric name prefix (without trailing underscore).
const Namespace = "smb_mount_healer"

// Label names. "namespace"/"pod" are deliberately avoided (Prometheus renames
// self-exported ones to exported_* unless honor_labels is set); "node" would
// clash with the scrape target's labels on the control-plane Deployment,
// hence target_node there.
const (
	LabelComponent         = "component"
	LabelNode              = "node"
	LabelTargetNode        = "target_node"
	LabelKind              = "kind"
	LabelResult            = "result"
	LabelPV                = "pv"
	LabelShare             = "share"
	LabelWorkloadNamespace = "workload_namespace"
	LabelWorkload          = "workload"
	LabelStatus            = "status"
	LabelFrom              = "from"
	LabelTo                = "to"
	LabelReason            = "reason"
	LabelAction            = "action"
	LabelDryRun            = "dry_run"
	LabelType              = "type"
	LabelStage             = "stage"
	LabelExit              = "exit"
	LabelMode              = "mode"
	LabelOutcome           = "outcome"
	LabelHealth            = "health"
	LabelVersion           = "version"
	LabelRevision          = "revision"
)

// Values of the component (and build_info mode) label.
const (
	ComponentWorker       = "worker"
	ComponentControlPlane = "controlplane"
)

// Values of the control-plane mode label.
const (
	ModeLive   = "live"
	ModeDryRun = "dry_run"
)

// Common result label values.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

// Worker metric names (const labels: component="worker", node).
const (
	MountChecksTotal                 = Namespace + "_mount_checks_total"                  // {kind,result}
	MountCheckDurationSeconds        = Namespace + "_mount_check_duration_seconds"        // {kind}
	MountStatus                      = Namespace + "_mount_status"                        // {kind,pv,share,workload_namespace,workload,status} enum gauge
	HealthTransitionsTotal           = Namespace + "_health_transitions_total"            // {kind,from,to}
	FalseAlarmsTotal                 = Namespace + "_false_alarms_total"                  // {kind,reason}
	ProbesHung                       = Namespace + "_probes_hung"                         // {}
	UnknownGlobalmounts              = Namespace + "_unknown_globalmounts"                // {}
	Quarantines                      = Namespace + "_quarantines"                         // {pv}
	QuarantineBytes                  = Namespace + "_quarantine_bytes"                    // {pv}
	QuarantineOldestTimestampSeconds = Namespace + "_quarantine_oldest_timestamp_seconds" // {pv}
	QuarantineScansTotal             = Namespace + "_quarantine_scans_total"              // {result}
	HostActionsTotal                 = Namespace + "_host_actions_total"                  // {action,result,dry_run}
	SmbMountStatusWritesTotal        = Namespace + "_smbmount_status_writes_total"        // {result}
)

// Control-plane metric names (const label: component="controlplane").
const (
	RemediationStage                = Namespace + "_remediation_stage"                  // {target_node,type,stage} enum gauge
	RemediationTransitionsTotal     = Namespace + "_remediation_transitions_total"      // {type,from,to,reason}
	RemediationStageDurationSeconds = Namespace + "_remediation_stage_duration_seconds" // {type,stage,exit}
	RemediationsStartedTotal        = Namespace + "_remediations_started_total"         // {target_node,type,mode}
	RemediationsTotal               = Namespace + "_remediations_total"                 // {target_node,type,mode,outcome}
	RemediationDurationSeconds      = Namespace + "_remediation_duration_seconds"       // {type,outcome}
	RemediationsResumedTotal        = Namespace + "_remediations_resumed_total"         // {stage}
	RemediationSuppressedTotal      = Namespace + "_remediation_suppressed_total"       // {reason}
	WorkloadActionsTotal            = Namespace + "_workload_actions_total"             // {action,result,mode}
	WorkloadVerificationsTotal      = Namespace + "_workload_verifications_total"       // {result}
	SmbMounts                       = Namespace + "_smbmounts"                          // {health}
	DryRun                          = Namespace + "_dry_run"                            // {} 1 = --dry-run, 0 = enforcing
)

// BuildInfo is exported by both modes: {component,version,revision,mode}.
const BuildInfo = Namespace + "_build_info"

// Label sets, in registration order. Tests assert these match the collectors.
var (
	MountChecksTotalLabels                 = []string{LabelKind, LabelResult}
	MountCheckDurationSecondsLabels        = []string{LabelKind}
	MountStatusLabels                      = []string{LabelKind, LabelPV, LabelShare, LabelWorkloadNamespace, LabelWorkload, LabelStatus}
	HealthTransitionsTotalLabels           = []string{LabelKind, LabelFrom, LabelTo}
	FalseAlarmsTotalLabels                 = []string{LabelKind, LabelReason}
	QuarantinesLabels                      = []string{LabelPV}
	QuarantineBytesLabels                  = []string{LabelPV}
	QuarantineOldestTimestampSecondsLabels = []string{LabelPV}
	QuarantineScansTotalLabels             = []string{LabelResult}
	HostActionsTotalLabels                 = []string{LabelAction, LabelResult, LabelDryRun}
	SmbMountStatusWritesTotalLabels        = []string{LabelResult}

	RemediationStageLabels                = []string{LabelTargetNode, LabelType, LabelStage}
	RemediationTransitionsTotalLabels     = []string{LabelType, LabelFrom, LabelTo, LabelReason}
	RemediationStageDurationSecondsLabels = []string{LabelType, LabelStage, LabelExit}
	RemediationsStartedTotalLabels        = []string{LabelTargetNode, LabelType, LabelMode}
	RemediationsTotalLabels               = []string{LabelTargetNode, LabelType, LabelMode, LabelOutcome}
	RemediationDurationSecondsLabels      = []string{LabelType, LabelOutcome}
	RemediationsResumedTotalLabels        = []string{LabelStage}
	RemediationSuppressedTotalLabels      = []string{LabelReason}
	WorkloadActionsTotalLabels            = []string{LabelAction, LabelResult, LabelMode}
	WorkloadVerificationsTotalLabels      = []string{LabelResult}
	SmbMountsLabels                       = []string{LabelHealth}

	BuildInfoLabels = []string{LabelVersion, LabelRevision, LabelMode}
)
