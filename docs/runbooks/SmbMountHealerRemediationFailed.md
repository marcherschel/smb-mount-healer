# SmbMountHealerRemediationFailed

## Metadata

| Field | Value |
| --- | --- |
| Alert Name | `SmbMountHealerRemediationFailed` |
| Alert Type | `Prometheus` |
| Severity | `warning` |

## Summary

An smb-mount-healer remediation ended without healing the mount.

{{ $value | printf "%.0f" }} smb-mount-healer remediation(s) on node {{ $labels.target_node }} ended with outcome {{ $labels.outcome }} in the past hour. The episode stopped without healing the mount, so the affected workloads may be scaled down, rolled back onto a still-broken mount, or writing to node-local disk. A human has to look at kubectl get smbr -l smbhealer.herschel.io/node={{ $labels.target_node }}. The alert clears one hour after the last failure.

## Impact

A remediation episode (`SmbMountRemediation`, `smbr`) of the Go smb-mount-healer reached `done` without healing the mount. Depending on the outcome, the affected workloads are:

- `verify_failed`: scaled back up, but at least one of them still has a broken mount, so it may be writing to node-local disk;
- `rolled_back`: restored to their original replicas on a mount that is probably still broken (aborted, deleted, `EPISODE_MAX_DURATION` of 30m reached, or every scoped workload deleted);
- `failed`: a host task failed (sweep) or an unrecoverable error ended the episode;
- `skipped_no_owner`: never touched, because the broken pods have no Deployment/StatefulSet owner the healer could scale.

Only live remediations count (`mode="live"`); dry-run episodes end with outcome `dry_run` and never fire this.

## Diagnosis

```bash
kubectl get smbr -o wide -l smbhealer.herschel.io/node=<target_node> --sort-by=.metadata.creationTimestamp
kubectl get smbr <name> -o jsonpath='{.status.message}{"\n"}'
kubectl get smbr <name> -o jsonpath='{.status.transitions}' | jq .   # which stage ended it, and why
kubectl get smbr <name> -o jsonpath='{.status.verification}' | jq .  # verify_failed: which workload
kubectl get smbr <name> -o jsonpath='{.status.conditions}' | jq .    # Aborted, ScopeConflict
kubectl get events -A --field-selector involvedObject.name=<name>
kubectl get smbm -l smbhealer.herschel.io/node=<target_node>         # is the mount still Broken?
kubectl get smbt -l smbhealer.herschel.io/remediation=<name>          # host tasks of the episode
kubectl get smbt <task> -o yaml                                      # per-action result and message
```

Per outcome:

- **`verify_failed`**: the new pods still saw a broken mount. Check `kubectl get smbm <name> -o yaml` (`.status.podMounts`) and the `csi-smb-node` driver logs on the node; an SMB server outage or bad credentials look like this. The next trigger starts a new episode after the backoff.
- **`rolled_back`**: look for the `rolling_back` transition reason: `aborted` (someone annotated it), `deleted`, `episode_timeout` (a stage hung; the transition before it shows which), `workloads_deleted`.
- **`failed`**: a `smbt` has a `Failed` result; its message says which action (force unmount, quarantine) failed. Then check the worker log on the node: `kubectl -n smb-mount-healer-system logs <worker-pod> --since=1h`.
- **`skipped_no_owner`**: list the pods on the node that use the PV (`.status.podMounts` of the `smbm`) and find the ones without a Deployment/StatefulSet owner.

## Mitigation

- **Mount still broken, workload affected**: scale every workload using the PV on that node to 0, wait until its globalmount directory under `/var/lib/kubelet/plugins/kubernetes.io/csi/smb.csi.k8s.io/<hash>/` is gone, restart the `csi-smb-node` pod on the node, then scale back up. Or cordon and drain the node so the pods land elsewhere.
- **`skipped_no_owner`**: delete the ownerless pods by hand (or give them an owner); the next episode can then heal.
- **SMB server down**: check the file server or the storage provider's status page; nothing to do on the cluster side.
- **Quarantined data**: if the episode moved data aside, `SmbMountHealerShadowDataPending` follows; recover it.
- Once the mount is healthy again (`kubectl get smbm` shows `Healthy`), nothing else is needed: the failed `smbr` stays as history and is garbage collected after `REMEDIATION_RETENTION` (24h, keeping at least `REMEDIATION_RETAIN_MIN_PER_NODE` = 10 per node).

## Notes

`for: 0m` on a counter: the alert fires on the first failed episode and clears one hour after the last one, because `increase(...[1h])` drops back to 0. It is a warning, because the healer retries on its own after the backoff (`BACKOFF_BASE` 5m doubling to `BACKOFF_MAX` 1h); repeated failures with workloads cycling show up as `SmbMountHealerRemediationLoop` (critical) instead.

A remediation that hangs is not alerted separately: `EPISODE_MAX_DURATION` (30m) forces a rollback, which ends as `rolled_back` and fires this alert.
