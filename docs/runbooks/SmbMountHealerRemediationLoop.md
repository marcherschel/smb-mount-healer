# SmbMountHealerRemediationLoop

## Metadata

| Field | Value |
| --- | --- |
| Alert Name | `SmbMountHealerRemediationLoop` |
| Alert Type | `Prometheus` |
| Severity | `critical` |

## Summary

smb-mount-healer is stuck in a remediation loop.

The smb-mount-healer started {{ $value | printf "%.0f" }} workload remediations for node {{ $labels.target_node }} in the past hour. A remediation that does not stick puts the workloads on the affected SMB volume into a scale-down/scale-up loop, and their pods may be writing to node-local disk instead of the SMB share. Check kubectl get smbr -l smbhealer.herschel.io/node={{ $labels.target_node }}.

## Impact

The smb-mount-healer keeps finding broken SMB (CIFS) mounts on this node, remediating them, and finding them broken again. Every workload remediation scales the affected Deployments/StatefulSets to 0 and back up, so those apps keep restarting. Worse, the usual cause is a "silent local dir" mount: the pods see an empty directory on the node's local disk instead of the SMB share, and anything they write there never reaches the share (it ends up in quarantine, see `SmbMountHealerShadowDataPending`).

Only `mode="live"` Workload remediations count, so a dry-run control plane never fires it.

## Diagnosis

Look at the mounts and episodes of the node (all four CRDs are cluster-scoped):

```bash
kubectl get smbm -l smbhealer.herschel.io/node=<node>            # health, reason, stage, last outcome per PV
kubectl get smbr -o wide -l smbhealer.herschel.io/node=<node> --sort-by=.metadata.creationTimestamp
kubectl get smbr <name> -o jsonpath='{.status.transitions}' | jq .   # every stage change with its reason
kubectl get smbr <name> -o jsonpath='{.status.scope}' | jq .         # workloads, PVs and drain pods in scope
kubectl get smbr <name> -o jsonpath='{.status.verification}' | jq .  # per-workload verified / elsewhere / failed
kubectl get events -A --field-selector involvedObject.name=<name>    # DrainTimeout, UnstagePinned, RollingBack, ...
```

Host actions (force unmount, quarantine, probes) the worker ran for an episode:

```bash
kubectl get smbt -l smbhealer.herschel.io/remediation=<name>
kubectl get smbt <task> -o yaml    # .status.results: one result per action, with message
```

Logs: `kubectl -n smb-mount-healer-system logs deploy/smb-mount-healer-controlplane --since=1h` and the worker pod on the node (`kubectl -n smb-mount-healer-system get pods -l app.kubernetes.io/component=worker -o wide`). The "SMB Mount Healer" Grafana dashboard shows the stage timeline, outcomes and transition reasons.

Read the transitions of consecutive episodes; the reasons tell the cause:

- **`unstage_timeout` + `UnstagePinned` events**: a pod still references the PV on that node, so kubelet never unstages and re-mounts the volume. The event names the pinning pods. Typical culprits are pods without a Deployment/StatefulSet owner (a bare pod, a Job, a DaemonSet) that the healer cannot scale down.
- **`scale_reassert` actions in the dashboard, or `scale_down_timeout`**: something scales the workloads back up mid-remediation, e.g. a GitOps controller correcting drift on `spec.replicas` (Flux HelmRelease `driftDetection` without `ignore: /spec/replicas`, Argo CD self-heal), a HorizontalPodAutoscaler or another operator. Find out who writes `spec.replicas` (`kubectl get deploy <name> --show-managed-fields -o yaml`).
- **`verify_timeout` with verification `failed`**: the share comes back broken after the driver restart, so the SMB server itself is likely unreachable or the credentials are wrong. Check the `csi-smb-node` driver logs (`-c smb`) on the node.
- **Quarantine actions on every episode** (`kubectl get smbq`): non-empty globalmount or orphaned pod dirs keep blocking the unstage.

## Mitigation

- **Stop a running episode**: `kubectl annotate smbr <name> smbhealer.herschel.io/abort=true` sends it to `rolling_back`, which scales every workload in scope back to its original replicas. Deleting the `smbr` does the same through its finalizer.
- **Pinning pods**: scale every workload that uses the PV on that node to 0 at once (or delete the ownerless pods), wait until the globalmount directory under `/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/<hash>/` is gone, then scale back up. The next episode then heals.
- **Drift correction**: add `driftDetection.ignore: [{paths: ["/spec/replicas"]}]` to the HelmRelease.
- **Shadow data**: `kubectl get smbq`, then merge the folder under `/var/lib/kubelet/smb-mount-healer-quarantine/` back into the share (see `SmbMountHealerShadowDataPending`).
- **SMB server down**: check the file server or the storage provider's status page. Backoff (`BACKOFF_BASE` 5m, doubling up to `BACKOFF_MAX` 1h) slows the loop down; it stops by itself once the share is reachable again.
- **Emergency brake**: switch the operator to dry run (chart value `dryRun: true`). It keeps detecting and planning but stops acting, and this alert goes quiet.

## Notes

Fires when one node gets more than 2 workload remediations within an hour, for 15 minutes. One isolated remediation is normal (a real CIFS hiccup); a stuck loop runs several per hour. Sweep remediations (orphaned dirs, no workload touched) are not counted.

Counters reset when the control plane restarts; `increase()` handles that, but the old pod's series still counts toward the 1h window, so the alert can stay active for up to an hour after a fix that involved restarting it.

Background: a GitOps controller that keeps restoring `spec.replicas` can pin a workload up while the healer scales it down, so the mount is never released and the workload loops. The healer re-asserts the scale-down (`scale_reassert`) in every stage for that reason, but it cannot win against a controller that reverts it every few seconds.
