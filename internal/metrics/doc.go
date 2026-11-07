// Package metrics is the metrics contract (names.go) and the collectors of
// both modes. Every metric is prefixed smb_mount_healer_ and carries
// component="worker"|"controlplane" as a const label; worker metrics also
// carry node as a const label, control-plane per-node metrics use target_node.
//
// Usage: build once in Run and register on controller-runtime's registry,
// which also serves controller_runtime_* and workqueue_*:
//
//	m := metrics.NewWorker(cfg.Worker.NodeName)
//	if err := m.Register(ctrlmetrics.Registry); err != nil { ... }
//	_ = metrics.RegisterBuildInfo(ctrlmetrics.Registry, metrics.ComponentWorker, version, revision)
//
// Tests use prometheus.NewRegistry() instead. Enum gauges (mount_status,
// remediation_stage) are maintained with SetEnum/DeleteEnum.
package metrics
