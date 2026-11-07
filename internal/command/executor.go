package command

import (
	"context"

	"github.com/marcherschel/smb-mount-healer/api/v1alpha1"
)

// ClusterExecutor carries out cluster commands (control plane). It is
// responsible for dry-run handling of ScaleWorkload/DeletePod and for the
// workload_actions_total metric. Implemented in internal/controlplane.
type ClusterExecutor interface {
	ExecuteCluster(ctx context.Context, cmd ClusterCommand) error
}

// HostExecutor carries out host commands (worker). It re-checks OnlyIf
// against live mountinfo, honours dryRun (result DryRun, nothing touched),
// and returns the result to record under cmd.ID(). Implemented in
// internal/worker.
type HostExecutor interface {
	ExecuteHost(ctx context.Context, dryRun bool, cmd HostCommand) v1alpha1.HostActionResult
}
