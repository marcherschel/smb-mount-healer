// Package controlplane is the --controlplane mode: a leader-elected
// controller-runtime manager running
//
//   - TriggerController (SmbMount changes -> create SmbMountRemediation,
//     backoff/scope-conflict suppression via remediation.Trigger),
//   - RemediationReconciler (remediation.Step, write status first, then
//     execute cluster commands; mirrors stage/outcome onto SmbMounts),
//   - GCController (retention, finalizer rollback),
//
// plus the cluster executor (command.ClusterExecutor over kube.Cluster,
// in executor.go). It never touches the host. RBAC is generated from the
// markers in rbac.go into config/rbac/role-controlplane.yaml.
package controlplane
