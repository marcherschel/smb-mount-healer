// Package inventory is the PURE builder of the worker's per-node view:
// PVs + pods on the node + MountSnapshot + kubelet directory facts ->
// NodeView (one VolumeView per smb PV with anything on this node, plus
// orphan pod dirs and globalmounts under unknown hashes).
//
// Collection is two-phase so Build stays pure: the worker lists the kubelet
// dirs, asks DirsToInspect which non-mountpoint dirs need DirFacts, inspects
// them through hostfs.DirInspector, then calls Build.
//
// Pod mounts of terminating pods or pods younger than POD_STARTUP_GRACE are
// marked Skip. PVs are matched to pods via spec.claimRef (no PVC reads).
// Outputs are sorted (PV name, then pod UID / path) for determinism.
//
// Purity (lint-enforced by forbidigo/depguard): no os, no time.Now/Sleep,
// no math/rand, no controller-runtime or client-go.
package inventory
