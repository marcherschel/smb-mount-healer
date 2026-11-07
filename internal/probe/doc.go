// Package probe is the self-exec subprocess layer. The worker runs
// "/proc/self/exe --probe <op> <path>" for stat, statfs and umount so a
// syscall stuck on a dead CIFS server never blocks the main process.
//
// Child protocol (RunChild): exactly one syscall, one JSON ChildResult line on
// stdout, exit code ExitResult; a failed syscall is reported in the JSON
// (errno + strerror), never via the exit code. ExitUsage means nothing ran.
//
// Parent (Runner): implements hostfs.Prober. Structural checks come from the
// MountSnapshot first (not_mountpoint, wrong_fstype), then a stat child is
// spawned with Pdeathsig=SIGKILL and killed after Timeout. A dedicated reaper
// goroutine waits for each child. At most one probe child and one umount
// child are alive per path; while one is alive further calls for that path
// return ProbeHung (or hostfs.ErrHung) without spawning. A child that has not
// exited KillGrace after SIGKILL is counted by Hung() until it finally dies.
package probe
