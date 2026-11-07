package remediation

import "github.com/marcherschel/smb-mount-healer/api/v1alpha1"

//go:generate go test -run TestStateMachineDocUpToDate -update-docs .

// Transition reasons. They are the bounded label set of
// remediation_transitions_total{reason} and appear in status.transitions[].
const (
	ReasonCreated              = "created"
	ReasonNoScopeConflict      = "no_scope_conflict"
	ReasonSuperseded           = "superseded"
	ReasonScopeConflict        = "scope_conflict"
	ReasonProbeCompleted       = "probe_completed"
	ReasonProbeTimeout         = "probe_timeout"
	ReasonNoOwner              = "no_owner"
	ReasonNothingToSweep       = "nothing_to_sweep"
	ReasonDrained              = "drained"
	ReasonScaleDownTimeout     = "scale_down_timeout"
	ReasonHostTaskCompleted    = "host_task_completed"
	ReasonHostTaskFailed       = "host_task_failed"
	ReasonNothingToDo          = "nothing_to_do"
	ReasonUnstaged             = "unstaged"
	ReasonForceUnmounted       = "force_unmounted"
	ReasonUnstageCompleted     = "unstage_completed"
	ReasonUnstageTimeout       = "unstage_timeout"
	ReasonDriverReady          = "driver_ready"
	ReasonDriverRestartTimeout = "driver_restart_timeout"
	ReasonNoDriverPod          = "no_driver_pod"
	ReasonScaledUp             = "scaled_up"
	ReasonVerified             = "verified"
	ReasonVerifyTimeout        = "verify_timeout"
	ReasonFinalized            = "finalized"
	ReasonDryRun               = "dry_run"
	ReasonDeleted              = "deleted"
	ReasonAborted              = "aborted"
	ReasonEpisodeTimeout       = "episode_timeout"
	ReasonWorkloadsDeleted     = "workloads_deleted"
	ReasonRestored             = "restored"
	ReasonRestoreTimeout       = "restore_timeout"
)

// Rule is one row of the transition table. Step only ever produces
// transitions that match a rule (tests enforce this), and
// docs/state-machine.md is rendered from Rules().
type Rule struct {
	// Machine is the remediation type the rule belongs to.
	Machine v1alpha1.RemediationType
	// From lists the source stages ("" is a freshly created object).
	From []v1alpha1.Stage
	To   v1alpha1.Stage
	// Reasons are the transition reasons this rule may record.
	Reasons []string
	// Guard describes when the rule fires.
	Guard string
	// Actions describes the commands emitted while waiting in From (or on the transition).
	Actions string
}

// workloadActiveStages are the Workload stages the global rollback guards apply to.
var workloadActiveStages = []v1alpha1.Stage{
	v1alpha1.StagePending, v1alpha1.StagePlanning, v1alpha1.StageDraining,
	v1alpha1.StageReleasingMounts, v1alpha1.StageAwaitingUnstage, v1alpha1.StageQuarantining,
	v1alpha1.StageAwaitingUnstageCompletion, v1alpha1.StageRestartingDriver,
	v1alpha1.StageScalingUp, v1alpha1.StageVerifying,
}

// sweepActiveStages are the Sweep stages the global rollback guards apply to.
var sweepActiveStages = []v1alpha1.Stage{
	v1alpha1.StagePlanning, v1alpha1.StageSweeping, v1alpha1.StageAwaitingUnstageCompletion,
}

const (
	holdZero  = "re-assert `Scale(0)` (`scale_reassert`) for any scoped workload above 0 replicas"
	mirrorAll = "mirror stage onto affected `SmbMount`s"
)

var rules = []Rule{
	// ---------------- Workload ----------------
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{""}, To: v1alpha1.StagePending,
		Reasons: []string{ReasonCreated}, Guard: "first Step of a new object", Actions: "set `startedTime`"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StagePending}, To: v1alpha1.StagePlanning,
		Reasons: []string{ReasonNoScopeConflict},
		Guard:   "no other active remediation's scope contains a candidate workload",
		Actions: "while conflicting: condition `ScopeConflict=True`, requeue `POLL_INTERVAL`"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StagePending}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonSuperseded},
		Guard:   "waited on a scope conflict, no broken pod of its own is left and every triggering `SmbMount` is Healthy again (outcome `healed`; a no-op that does not count towards backoff)",
		Actions: "while it waited and has no broken pod yet: stay `pending` (the other remediation drained its pods; the replacements are planned once they break)"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StagePlanning}, To: v1alpha1.StageDraining,
		Reasons: []string{ReasonProbeCompleted, ReasonProbeTimeout, ReasonDryRun},
		Guard:   "`ProbeNow` host task Succeeded/Failed, or `PLANNING_PROBE_WAIT`, or dry run",
		Actions: "on entry: `CreateHostTask(ProbeNow)` for every globalmount of the node; scope (owners of broken pods + sibling PVs + co-tenants of non-healthy globalmounts) with `originalReplicas` from the live spec; on exit: re-plan with fresh health"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StagePlanning}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonNoOwner},
		Guard:   "re-planned scope has no Deployment/StatefulSet (outcome `skipped_no_owner`)"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StagePlanning}, To: v1alpha1.StagePending,
		Reasons: []string{ReasonScopeConflict},
		Guard:   "re-planned scope overlaps an older active remediation (tie-break: creation time, name)",
		Actions: "clear scope"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageDraining}, To: v1alpha1.StageReleasingMounts,
		Reasons: []string{ReasonDrained, ReasonScaleDownTimeout, ReasonDryRun},
		Guard:   "every drain pod gone and no pod on the node uses a co-tenant PV, or `SCALE_DOWN_WAIT` (Warning `DrainTimeout`), or dry run",
		Actions: "`Scale(0)` (`scale_down`, then `scale_reassert` once observed at 0); add newly appearing owned co-tenant pods/workloads to scope"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageReleasingMounts}, To: v1alpha1.StageAwaitingUnstage,
		Reasons: []string{ReasonHostTaskCompleted, ReasonNothingToDo, ReasonDryRun},
		Guard:   "host task Succeeded/Failed (failures are best effort), no mounted drain-pod path, or dry run (task exists)",
		Actions: "`CreateHostTask(ForceUnmount pod paths of drain pods, onlyIf=NotHealthyMount)`; " + holdZero},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageAwaitingUnstage}, To: v1alpha1.StageQuarantining,
		Reasons: []string{ReasonNothingToDo, ReasonUnstaged, ReasonForceUnmounted, ReasonDryRun},
		Guard:   "no dead globalmount, or all dead globalmounts unmounted, or after `UNSTAGE_WAIT` the force-unmount host task finished, or dry run (task exists)",
		Actions: "on entry: record `deadGlobalmounts`; after `UNSTAGE_WAIT`: `CreateHostTask(ForceUnmount dead globalmounts)`; " + holdZero},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageQuarantining}, To: v1alpha1.StageAwaitingUnstageCompletion,
		Reasons: []string{ReasonHostTaskCompleted, ReasonNothingToDo, ReasonDryRun},
		Guard:   "host task Succeeded/Failed, nothing to quarantine, or dry run (task exists)",
		Actions: "`CreateHostTask` over pod dirs of any pod UID and the globalmount dir of every scoped PV: `ForceUnmount` (onlyIf=NotHealthyMount) + `Quarantine` (onlyIf=NotMountpoint); " + holdZero},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageAwaitingUnstageCompletion}, To: v1alpha1.StageRestartingDriver,
		Reasons: []string{ReasonUnstageCompleted, ReasonUnstageTimeout, ReasonDryRun},
		Guard:   "no scoped globalmount dir left unmounted, or `QUARANTINE_UNSTAGE_WAIT` (Warning `UnstagePinned` naming the pinning pods), or dry run",
		Actions: holdZero},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageRestartingDriver}, To: v1alpha1.StageScalingUp,
		Reasons: []string{ReasonDriverReady, ReasonDriverRestartTimeout, ReasonNoDriverPod, ReasonDryRun},
		Guard:   "a driver pod with a new UID is Running+Ready, or `DRIVER_RESTART_WAIT`, or no driver pod found, or dry run",
		Actions: "record `driver.oldPod*` (written before the delete), then `DeletePod{uid}` (`driver_restart`) while the old pod exists; " + holdZero},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageScalingUp}, To: v1alpha1.StageVerifying,
		Reasons: []string{ReasonScaledUp, ReasonDryRun},
		Guard:   "every existing scoped workload has replicas == originalReplicas, or dry run",
		Actions: "`Scale(originalReplicas)` (`scale_up`)"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageVerifying}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonVerified, ReasonVerifyTimeout, ReasonDryRun},
		Guard:   "every workload `verified` (new pods' podMounts Healthy) or `elsewhere` (no new pod after `VERIFY_NO_POD_GRACE`), or `VERIFY_WAIT` (rest `failed`), or dry run",
		Actions: "`CreateHostTask(ProbeNow)` for scoped globalmounts, and once for the new pods' mount paths"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: workloadActiveStages, To: v1alpha1.StageRollingBack,
		Reasons: []string{ReasonDeleted, ReasonAborted, ReasonEpisodeTimeout, ReasonWorkloadsDeleted},
		Guard:   "deletionTimestamp set (finalizer), abort annotation, `EPISODE_MAX_DURATION` since start, or every scoped workload deleted (draining..verifying)",
		Actions: "Warning `RollingBack`; condition `Aborted` on abort"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageRollingBack}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonRestored, ReasonRestoreTimeout, ReasonDryRun},
		Guard:   "every not-yet-restored workload back at originalReplicas (or deleted), or `SCALE_DOWN_WAIT` (Warning `RestoreTimeout`), or dry run (outcome `rolled_back`)",
		Actions: "`Scale(originalReplicas)` (`rollback`)"},
	{Machine: v1alpha1.RemediationTypeWorkload, From: []v1alpha1.Stage{v1alpha1.StageFinalizing}, To: v1alpha1.StageDone,
		Reasons: []string{ReasonFinalized},
		Guard:   "always",
		Actions: "Event `RemediationFinished`; mirror `lastRemediation` + conditions onto affected `SmbMount`s"},

	// ---------------- Sweep ----------------
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{""}, To: v1alpha1.StagePlanning,
		Reasons: []string{ReasonCreated}, Guard: "first Step of a new object", Actions: "set `startedTime`"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StagePlanning}, To: v1alpha1.StageSweeping,
		Reasons: []string{ReasonProbeCompleted, ReasonProbeTimeout, ReasonDryRun},
		Guard:   "`ProbeNow` host task Succeeded/Failed, or `PLANNING_PROBE_WAIT`, or dry run; then orphan findings remain on unreferenced PVs",
		Actions: "on entry: `CreateHostTask(ProbeNow)`; on exit: scope = triggering PVs with orphan findings and no pod on the node, `deadGlobalmounts` = their Broken mounted globalmounts"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StagePlanning}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonNothingToSweep},
		Guard:   "fresh health shows no orphan finding left (outcome `healed`)"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StageSweeping}, To: v1alpha1.StageAwaitingUnstageCompletion,
		Reasons: []string{ReasonHostTaskCompleted, ReasonNothingToDo, ReasonDryRun},
		Guard:   "host task Succeeded, nothing to do, or dry run (task exists)",
		Actions: "`CreateHostTask`: `ForceUnmount` dead orphan globalmounts and mounted orphan pod dirs (onlyIf=NotHealthyMount), `Quarantine` orphan pod dirs and globalmount dirs (onlyIf=NotMountpoint)"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StageSweeping}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonHostTaskFailed},
		Guard:   "host task Failed (outcome `failed`)"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StageAwaitingUnstageCompletion}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonUnstageCompleted, ReasonUnstageTimeout, ReasonDryRun},
		Guard:   "no scoped globalmount dir left unmounted, or `QUARANTINE_UNSTAGE_WAIT` (Warning `UnstagePinned`), or dry run (outcome `healed` / `dry_run`)"},
	{Machine: v1alpha1.RemediationTypeSweep, From: sweepActiveStages, To: v1alpha1.StageRollingBack,
		Reasons: []string{ReasonDeleted, ReasonAborted, ReasonEpisodeTimeout},
		Guard:   "deletionTimestamp set, abort annotation, or `EPISODE_MAX_DURATION`",
		Actions: "Warning `RollingBack`"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StageRollingBack}, To: v1alpha1.StageFinalizing,
		Reasons: []string{ReasonRestored, ReasonDryRun},
		Guard:   "nothing to restore (outcome `rolled_back`)"},
	{Machine: v1alpha1.RemediationTypeSweep, From: []v1alpha1.Stage{v1alpha1.StageFinalizing}, To: v1alpha1.StageDone,
		Reasons: []string{ReasonFinalized},
		Guard:   "always",
		Actions: "Event `RemediationFinished`; " + mirrorAll},
}

// Rules returns a copy of the transition table of both machines, in
// documentation order.
func Rules() []Rule {
	out := make([]Rule, len(rules))
	for i, r := range rules {
		r.From = append([]v1alpha1.Stage(nil), r.From...)
		r.Reasons = append([]string(nil), r.Reasons...)
		out[i] = r
	}
	return out
}

// RuleIndex returns the index into Rules() of the rule matching a
// transition, or -1 if no rule matches.
func RuleIndex(machine v1alpha1.RemediationType, from, to v1alpha1.Stage, reason string) int {
	for i, r := range rules {
		if r.Machine != machine || r.To != to {
			continue
		}
		if !containsStage(r.From, from) {
			continue
		}
		for _, rr := range r.Reasons {
			if rr == reason {
				return i
			}
		}
	}
	return -1
}

func containsStage(list []v1alpha1.Stage, s v1alpha1.Stage) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
