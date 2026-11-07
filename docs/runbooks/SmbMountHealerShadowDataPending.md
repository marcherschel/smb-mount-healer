# SmbMountHealerShadowDataPending

## Metadata

| Field | Value |
| --- | --- |
| Alert Name | `SmbMountHealerShadowDataPending` |
| Alert Type | `Prometheus` |
| Severity | `warning` |

## Summary

Data written while an SMB mount was dead is waiting in quarantine.

Node {{ $labels.node }} holds {{ $value | printf "%.0f" }} quarantine folder(s) for PV {{ $labels.pv }}. Pods wrote this data to the node's local disk while the SMB mount was dead, and the healer moved it aside instead of deleting it. It never reaches the SMB share on its own: list it with kubectl get smbq and merge it back by hand.

## Impact

While an SMB (CIFS) mount was dead, pods on this node wrote into the empty directory underneath the mountpoint, which is the node's local disk. During remediation the healer moved that data out of the way into a quarantine folder instead of deleting it, so the mount could be re-staged. This data is **not** on the SMB share: the app's view of the share is missing whatever it wrote during the outage (downloads, uploads, database files, ...) until someone merges it back. It also fills the node's disk.

The healer never deletes or merges quarantined data by itself.

## Diagnosis

```bash
kubectl get smbq                                      # one object per quarantine folder: node, PV, files, size, age
kubectl get smbq <name> -o yaml                       # .spec.path (folder on the node), .spec.sourcePath, .spec.remediationRef
kubectl get smbr <remediationRef> -o wide             # the episode that quarantined it (empty for adopted folders)
```

`SmbQuarantine` only carries counts and sizes, never file names. The healer image is distroless (no shell), so look at the content from a node debug pod, which mounts the host root at `/host` (Talos needs the `sysadmin` profile):

```bash
kubectl debug -n kube-system node/<node> -it --profile=sysadmin --image=busybox:1.37 -- ls -laR /host<spec.path>
```

The folder lives under `<kubeletRoot>/smb-mount-healer-quarantine/` (`/var/lib/kubelet/smb-mount-healer-quarantine/` with the default `kubeletRoot`). Folders in that directory that no remediation created (for example moved there by hand) are reported too, without a `remediationRef`.

## Mitigation

1. Find out which app owns the PV (`kubectl get pv <pv> -o jsonpath='{.spec.claimRef}'`) and whether the quarantined files are worth keeping. Compare them with what is on the share now.
2. Copy anything worth keeping back onto the share, e.g. from the node debug pod above into a pod that mounts the same PV (`kubectl cp` out of the debug pod, then into the app's pod), or straight to the file server over SMB.
3. Move the folder out of the quarantine directory or delete it on the node (from the debug pod: `rm -rf /host<spec.path>`). The `smbq` disappears on the next quarantine scan (`QUARANTINE_SCAN_INTERVAL`, 5m) once the folder is gone or empty, and the alert clears.

Deleting the `smbq` object does nothing: it reflects the disk and is recreated while the data remains.

## Notes

`for: 1h` gives the person handling the remediation time to recover the data before this pages. The quarantine gauge is exported by the worker on each node (`smb_mount_healer_quarantines{node, pv}`), so a worker that is down hides pending data; `kubectl get smbq` still lists it.
