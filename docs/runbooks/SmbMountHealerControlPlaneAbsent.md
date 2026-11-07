# SmbMountHealerControlPlaneAbsent

## Metadata

| Field | Value |
| --- | --- |
| Alert Name | `SmbMountHealerControlPlaneAbsent` |
| Alert Type | `Prometheus` |
| Severity | `critical` |

## Summary

The smb-mount-healer control plane is not reporting.

No smb-mount-healer control plane has reported smb_mount_healer_build_info for 30 minutes. Without the control plane, broken SMB mounts are still detected by the workers but nothing remediates them. absent() is sensitive to ingestion lag: with remote_write, a backed-up queue delays the series, so rule out lag first.

## Impact

No smb-mount-healer control plane reports metrics. The workers on each node still probe the SMB mounts and publish `SmbMount` health, but nothing turns a Broken mount into a remediation: dead SMB mounts stay dead, and pods keep writing to node-local disk until someone intervenes. Remediations that were in progress are frozen at their current stage (with their workloads possibly scaled to 0) until a control plane resumes them.

## Diagnosis

First rule out a monitoring problem: `absent()` is evaluated against real time, so when scraping fails or a remote_write queue falls behind (Grafana Alloy, Prometheus agent, ...), the series exists but has not reached the rule evaluator yet. If other `absent()` or scrape-target alerts fire at the same time, this alert is a symptom.

Then check the control plane itself:

```bash
kubectl -n smb-mount-healer-system get deploy,pods -l app.kubernetes.io/component=controlplane -o wide
kubectl -n smb-mount-healer-system describe pod -l app.kubernetes.io/component=controlplane
kubectl -n smb-mount-healer-system logs deploy/smb-mount-healer-controlplane --since=1h
kubectl -n smb-mount-healer-system get lease                  # leader election
kubectl get smbr -o wide                                      # episodes stuck in a non-terminal stage
```

If the pod runs but no metrics arrive, check the scrape: the `PodMonitor` (`metrics.podMonitor.enabled`, plus `metrics.podMonitor.labels` matching your Prometheus' `podMonitorSelector`) and the `smb-mount-healer-controlplane-metrics` service, e.g. `kubectl -n smb-mount-healer-system port-forward svc/smb-mount-healer-controlplane-metrics 8080` and `curl -s localhost:8080/metrics | grep smb_mount_healer_build_info`.

## Mitigation

- **Pod crash-looping**: the log shows the reason; config errors (an invalid env value) fail at start. Fix the Helm values (`helm upgrade ... --reuse-values --set ...`, or wherever your GitOps tool keeps them). The CRDs must be installed (`crds.install`) and RBAC in place.
- **Pod pending**: check node resources and the scheduling constraints in the values.
- **Release not deployed**: `helm -n <namespace> status smb-mount-healer` (with GitOps, e.g. Flux: `flux get helmreleases -A`).
- **Workloads left at 0 by a frozen episode**: once the control plane is back it resumes the episode on its own. If it cannot come back soon, restore the workloads by hand to the `originalReplicas` in `kubectl get smbr <name> -o jsonpath='{.status.scope.workloads}'`.

## Notes

`for: 30m` (more than the usual 15m for `absent()`) because remote-write lag regularly causes short false `absent()` episodes in this cluster. A missing worker is not alerted here; the generic DaemonSet alerts cover it.
