// Package linux implements hostfs on Linux for the worker:
//
//   - MountTable: /proc/self/mountinfo via moby/sys/mountinfo, prefix
//     filtered on the kubelet root, in file order (last entry wins).
//   - DirInspector: reads the kubelet tree with raw getdents only. It never
//     stats or reads a path that is a mountpoint (strictly below the root);
//     existence and type come from the parent's directory entries.
//   - Mutator: ForceUnmount through the probe child (umount2 with
//     MNT_FORCE|MNT_DETACH); Quarantine renames each top-level entry
//     (dotfiles included) into a new folder, with copy+remove on EXDEV,
//     refuses live mountpoints (fresh mountinfo) and never deletes anything.
//   - QuarantineInspector: lists quarantine folders and walks them with an
//     entry and time budget, never following symlinks, returning aggregates.
//
// Privacy: no name of an entry inside a quarantined directory ever appears
// in a returned value or error; errors about such entries carry only the
// operation and the errno.
package linux
