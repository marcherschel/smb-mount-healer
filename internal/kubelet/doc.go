// Package kubelet holds the pure path math of the kubelet directory layout:
//
//	globalmount:  <root>/plugins/kubernetes.io/csi/<driver>/<sha256hex(volumeHandle)>/globalmount
//	pod volume:   <root>/pods/<uid>/volumes/kubernetes.io~csi/<pv>/mount
//	quarantine:   <QUARANTINE_DIR>/<pv>-<YYYYMMDDTHHMMSSZ>[-<n>]   (bash healer format; -<n> on collision)
//
// The sha256 derivation matches what the bash healer verified against the cluster.
package kubelet
