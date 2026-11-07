// Package remediation is the PURE remediation state machine.
//
// Step(obj, observation, cfg) -> Result{status', commands, transitions,
// requeue}. The control plane writes Result.Status BEFORE executing
// Result.Commands (write-ahead); because every command is idempotent and all
// deadlines are absolute timestamps in status, re-running Step after a crash
// or leader change converges to the same result.
//
// Two machines share the Stage enum:
//   - Workload: pending -> planning -> draining -> releasing_mounts ->
//     awaiting_unstage -> quarantining -> awaiting_unstage_completion ->
//     restarting_driver -> scaling_up -> verifying -> finalizing -> done
//   - Sweep: planning -> sweeping -> awaiting_unstage_completion -> finalizing -> done
//
// Any non-terminal stage goes to rolling_back on EPISODE_MAX_DURATION, the
// abort annotation, deletion, or every scoped workload being deleted.
// Trigger decides whether a new remediation should be created for a node
// (backoff from history, scope conflicts).
//
// Purity (lint-enforced by forbidigo/depguard): no os, no time.Now/Sleep,
// no math/rand, no controller-runtime or client-go. Time comes from
// Observation.Now. Output order must be deterministic.
package remediation
