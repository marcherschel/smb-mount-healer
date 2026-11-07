// Package hostfs defines the worker's view of the node's filesystem: the
// mount table, directory facts, mount probes, host mutations and the
// quarantine inspector. This root package holds only types, interfaces and
// pure helpers (MountSnapshot lookups); it is lint-enforced pure (no os,
// no time.Now, no controller-runtime).
//
// Implementations:
//   - hostfs/linux: the real thing, worker only.
//   - hostfs/fake: an in-memory fake for unit tests and internal/sim.
//
// Syscall rules every implementation must follow:
//   - Read /proc/self/mountinfo only; never stat, lstat or readdir a
//     mountpoint inside the main process.
//   - stat, statfs and umount run in a self-exec'd child (internal/probe),
//     killed on timeout. A child stuck in D-state marks the path hung and no
//     new probe of that path is spawned.
//   - Nothing is ever deleted. Quarantine moves entries with rename (copy and
//     remove on EXDEV) into a new folder.
//   - Nothing returned by QuarantineInspector carries an entry name from
//     quarantined data.
package hostfs
