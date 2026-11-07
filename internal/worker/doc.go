// Package worker is the --worker mode: a per-node controller-runtime
// manager (no leader election) running
//
//   - ProberRunnable (inventory.Build -> probes with bounded concurrency ->
//     health.Step), cadence CHECK_INTERVAL / RECHECK_DELAY / ProbeNow,
//   - SmbMountPublisher (create SmbMount, SSA the worker's status fields with
//     v1alpha1.FieldManagerWorker, heartbeat rate limiting, cleanup),
//   - HostTaskReconciler (runs SmbHostTask actions via the host executor),
//   - QuarantineScanner (SmbQuarantine objects follow the disk),
//
// plus the host executor (command.HostExecutor over hostfs, in
// hostexecutor.go). It makes no remediation decisions. RBAC is generated from
// rbac.go into config/rbac/role-worker.yaml.
package worker
