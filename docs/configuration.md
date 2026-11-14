# Configuration

The Helm chart ([values](../production/smb-mount-healer/README.md#values))
maps its values onto the flags and environment variables below; set them
directly only when you run the binary without the chart.


Flags:

| Flag | Default | Purpose |
|---|---|---|
| `--controlplane` / `--worker` | | Mode (exactly one). |
| `--dry-run` | | Control plane only. Same effect as `DRY_RUN=true`. |
| `--leader-elect` | `true` | Control plane leader election. |
| `--metrics-bind-address` | `:8080` | Metrics endpoint. |
| `--health-probe-bind-address` | `:8081` | Health endpoint. |
| `--zap-*` | | controller-runtime logging. |

All other settings come from environment variables. A duration can be plain
seconds (`300`) or a Go duration (`5m`).

Both modes:

| Variable | Default |
|---|---|
| `CSI_DRIVER_NAME` | `smb.csi.k8s.io` |

Worker:

| Variable | Default |
|---|---|
| `NODE_NAME` | (required) |
| `KUBELET_ROOT` | `/var/lib/kubelet` |
| `EXPECTED_FSTYPES` | `cifs smb3` |
| `CHECK_INTERVAL` | 60s |
| `RECHECK_ATTEMPTS` | 3 |
| `RECHECK_DELAY` | 3s |
| `STAT_TIMEOUT` | 5s |
| `STARTUP_GRACE` | 10s |
| `POD_STARTUP_GRACE` | 60s |
| `SLOW_CONFIRM_WINDOW` | 3m |
| `SLOW_RECOVERY_WINDOW` | 2m |
| `PROBE_CONCURRENCY` | 4 |
| `STATUS_HEARTBEAT` | 5m |
| `QUARANTINE_DIR` | `$KUBELET_ROOT/smb-mount-healer-quarantine` |
| `QUARANTINE_SCAN_INTERVAL` | 5m |
| `QUARANTINE_SCAN_MAX_ENTRIES` | 100000 |
| `QUARANTINE_SCAN_TIME_BUDGET` | 10s |

Control plane:

| Variable | Default |
|---|---|
| `DRY_RUN` | false |
| `CSI_NODE_DRIVER_NAMESPACE` | `kube-system` |
| `CSI_NODE_DRIVER_LABEL_SELECTOR` | `app=csi-smb-node` |
| `POLL_INTERVAL` | 3s |
| `PLANNING_PROBE_WAIT` | 15s |
| `SCALE_DOWN_WAIT` | 120s |
| `UNSTAGE_WAIT` | 30s |
| `QUARANTINE_UNSTAGE_WAIT` | 300s |
| `DRIVER_RESTART_WAIT` | 90s |
| `VERIFY_WAIT` | 90s |
| `VERIFY_NO_POD_GRACE` | 30s |
| `EPISODE_MAX_DURATION` | 30m |
| `BACKOFF_BASE` | 5m |
| `BACKOFF_MAX` | 1h |
| `REMEDIATION_RETENTION` | 24h |
| `REMEDIATION_RETAIN_MIN_PER_NODE` | 10 |
| `LEADER_ELECTION_NAMESPACE` | `$POD_NAMESPACE` |
| `LEADER_ELECTION_ID` | `smb-mount-healer-controlplane` |
