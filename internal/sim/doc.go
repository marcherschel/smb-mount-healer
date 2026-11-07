// Package sim is the simulated world: a fake kube API with a replica
// emulator, a fake kubelet with the incident semantics (stage is a no-op on
// an existing mountpoint, publish binds the bare globalmount dir, unpublish
// and unstage fail on non-empty dirs, unstage retry backoff capped at
// 2m10s), a fake samba and a fake clock. Both modes run against it and emit
// JSONL golden traces (make sim-update rewrites them).
package sim
