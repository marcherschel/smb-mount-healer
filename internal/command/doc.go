// Package command defines the typed, idempotent commands the pure state
// machines emit, and the executor interfaces that carry them out.
//
//   - ClusterCommand: executed by the control plane's cluster executor
//     (internal/controlplane) through internal/kube.
//   - HostCommand: executed by the worker's host executor (in
//     internal/worker) through internal/hostfs.
//
// Every command is idempotent: executing it twice has the same effect as
// once, so re-running a Step after a crash or leader change is safe. Key()
// is a stable identity used for de-duplication and deterministic ordering.
//
// This package is lint-enforced pure: data types and interfaces only, no os,
// no time.Now, no controller-runtime.
package command
