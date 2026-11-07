// Package v1alpha1 contains the smbhealer.herschel.io/v1alpha1 API: the four
// cluster-scoped custom resources through which the smb-mount-healer worker
// (DaemonSet) and control plane (Deployment) share all of their state.
//
//   - SmbMount (smbm): one object per (node, PV). Spec is written once by the
//     worker; status is split between the worker (health) and the control
//     plane (remediation) by server-side-apply field managers.
//   - SmbMountRemediation (smbr): one remediation episode. Written only by the
//     control plane; its status IS the remediation state machine's state.
//   - SmbHostTask (smbt): host actions the control plane asks one worker to run.
//   - SmbQuarantine (smbq): one quarantine folder on a node. Written only by
//     the worker, always reflecting what is on disk. It never carries file or
//     directory names from the quarantined data.
//
// +kubebuilder:object:generate=true
// +groupName=smbhealer.herschel.io
// +kubebuilder:ac:generate=true
package v1alpha1
