# Failure modes and limits

This page lists the SMB failures smb-mount-healer is built for. For each one
it says what the healer detects, what it does, how the episode usually ends,
and what it deliberately leaves alone. The last section,
[When not to use this](#when-not-to-use-this), covers workloads where the
healer's one tool, scaling the owning workload to 0 and back, costs more than
the broken mount.

Each entry names the e2e test (`test/e2e`, `TestS<n>…`) or simulator scenario
(`internal/sim/scenarios.go`) that tests it. An entry without one says so.

## How to read this

**Detection.** The worker probes every SMB globalmount and pod mount every
`CHECK_INTERVAL` (60s) and debounces the result per mount:

| Reason | Class | Becomes `Broken` |
|---|---|---|
| `not_mountpoint`, `wrong_fstype`, `stat_error` | fast / structural | after `RECHECK_ATTEMPTS` (3) failed rechecks, `RECHECK_DELAY` (3s) apart |
| `stat_timeout`, `probe_hung` | slow | only once failures have lasted `SLOW_CONFIRM_WINDOW` (3m) |
| `probe_error` | probe machinery failed | never: the mount is `Errored` and is not remediated |

A mount that recovers while `Suspect` is a false alarm. After slow failures
it must probe ok for `SLOW_RECOVERY_WINDOW` (2m) to count as recovered, so a
mount that keeps flapping is still confirmed.

**Outcomes** of an `SmbMountRemediation`: `healed`, `verify_failed`,
`skipped_no_owner`, `rolled_back`, `failed`, `dry_run`. The stages are
specified in [state-machine.md](state-machine.md).

**Retries.** An episode never retries itself. A mount that is still broken
triggers a new episode after the backoff, which is per PV and starts at
`BACKOFF_BASE` (5m), doubling up to `BACKOFF_MAX` (1h). Every episode rolls
back after `EPISODE_MAX_DURATION` (30m).

## Catalogue

| Failure | Detected as | Usual outcome | Tested by |
|---|---|---|---|
| [Short blip](#short-blip) | `Suspect`, then `Healthy` | nothing happens | e2e S2, sim `s02-transient-blip` |
| [Slow or paused server](#slow-or-paused-server) | `stat_timeout` / `probe_hung`, stays `Suspect` | nothing happens | e2e S3, sim `s03-slow-server` |
| [Hung server](#hung-server) | `stat_timeout` / `probe_hung` for > 3m | `healed` | sim `s03b-hung-server` |
| [Flapping or degraded client](#flapping-or-degraded-client) | slow failures between ok probes | `healed` | e2e S3 netem, sim `s03c`, `s03d` |
| [SMB server reboot](#smb-server-reboot) | `stat_error` / `stat_timeout`, or `wrong_fstype` | `healed`; shadow writes quarantined | e2e S4, S5, S6, S13 |
| [IP change](#ip-change) | `stat_error` / `stat_timeout` | `healed` | e2e S13, sim `s13-samba-new-ip` |
| [Expired or rotated credentials](#expired-or-rotated-credentials) | typically `stat_error`, once the session reconnects | `verify_failed`, repeated behind backoff | **none** |
| [Partial outage: one share](#partial-outage-one-share) | only that PV's mounts | `healed`; other PVs untouched | e2e S9, S11 |
| [Partial outage: server still down during the episode](#partial-outage-server-still-down-during-the-episode) | as above, then verification fails | `verify_failed`, then backoff | e2e S14, sim `s14-outage-during-episode` |
| [Partial outage: several nodes at once](#partial-outage-several-nodes-at-once) | one trigger per node | one episode per scope at a time | sim `x01-two-node-scope-contention` |
| [Leftover data on a dead or orphaned path](#leftover-data-on-a-dead-or-orphaned-path) | `globalmount_not_empty`, `orphan_*` | Sweep: quarantined, nothing scaled | e2e S6, S7, S8 |
| [A pod the healer can't scale pins the volume](#a-pod-the-healer-cant-scale-pins-the-volume) | `unstage_timeout`, `UnstagePinned` | `verify_failed` until a human acts | e2e S15 |
| [Something scales the workload back up](#something-scales-the-workload-back-up) | `scale_reassert`, `scale_down_timeout` | `healed` once it stops | e2e S10 |
| [The healer itself crashes](#the-healer-itself-crashes) | – | episode resumes | e2e S12, S12a, S12b; sim `cp-crash-*`, `x02-leader-change` |

### Short blip

- **Symptom:** a few seconds of `stat` errors or timeouts, then the mount works.
- **Detected as:** `Suspect`. The next ok probe makes it `Healthy` again and
  counts a false alarm (`smb_mount_healer_false_alarms_total`).
- **Does:** nothing.
- **Does not:** start a remediation for anything shorter than the recheck
  window (fast class) or `SLOW_CONFIRM_WINDOW` (slow class).
- **Tested by:** e2e `TestS2TransientBlip` (smbd paused for 7s), sim `s02-transient-blip`.

### Slow or paused server

- **Symptom:** `stat` takes longer than `STAT_TIMEOUT` (5s), or hangs, for
  less than `SLOW_CONFIRM_WINDOW`.
- **Detected as:** `stat_timeout` or `probe_hung`, which stays `Suspect`. Probes
  run in a child process, so a hung probe never blocks the worker.
- **Does:** nothing. The mount recovers on its own.
- **Tested by:** e2e `TestS3SlowServer` (smbd paused for 20s), sim `s03-slow-server`.

### Hung server

- **Symptom:** the server stops answering for longer than `SLOW_CONFIRM_WINDOW`.
  Processes touching the mount end up in D-state, and kubelet's own unmount of
  the pod path hangs too.
- **Detected as:** `stat_timeout` / `probe_hung`, `Broken` after 3m.
- **Does:** a full Workload episode. Draining hits `SCALE_DOWN_WAIT` because
  kubelet can't unmount, so `releasing_mounts` force-unmounts the pod paths,
  `awaiting_unstage` force-unmounts the dead globalmount, the CSI node driver
  restarts, and the workloads scale back up.
- **Outcome:** `healed` once the server answers again. While it doesn't, see
  [server still down during the episode](#partial-outage-server-still-down-during-the-episode).
- **Tested by:** sim `s03b-hung-server`.

### Flapping or degraded client

- **Symptom:** the CIFS client keeps timing out now and then, with good probes
  in between, long after the original fault is gone. This was seen on CI
  runners after a network delay.
- **Detected as:** slow failures. `SLOW_RECOVERY_WINDOW` keeps the failure
  streak across the good probes, so the mount becomes `Broken` once its first
  failure is `SLOW_CONFIRM_WINDOW` old instead of flapping forever.
- **Does:** a full Workload episode. The remount (driver restart and re-stage)
  creates a fresh CIFS session.
- **Tested by:** e2e `TestS3SlowServerNetem` (needs `sch_netem`), sim
  `s03c-flapping-server`; sim `s03d-flapping-server-no-recovery-window` shows
  that the mount is never healed with `SLOW_RECOVERY_WINDOW=0`.

### SMB server reboot

A reboot can end in one of three states on the node, and the healer handles
each differently:

1. **The kernel reconnects by itself.** The healer sees a
   [short blip](#short-blip) or a [slow server](#slow-or-paused-server) and
   does nothing.
2. **The session stays dead.** `stat` fails (`stat_error`) or hangs
   (`stat_timeout`/`probe_hung`). The healer runs a full Workload episode: plan
   the scope, scale to 0, force-unmount the dead mounts, restart
   `csi-smb-node`, scale up and verify. If the server comes back with a new
   address, see [IP change](#ip-change).
3. **The mount silently disappeared.** The globalmount was detached and a
   recreated pod got the bare directory underneath bind-mounted
   (`wrong_fstype` or `not_mountpoint`), so it writes to the node's disk. The
   healer runs the same episode, and `quarantining` moves anything written to
   local disk into `<kubeletRoot>/smb-mount-healer-quarantine/`. That
   creates an `SmbQuarantine`, and
   [`SmbMountHealerShadowDataPending`](runbooks/SmbMountHealerShadowDataPending.md)
   fires.

- **Outcome:** `healed`, once the server is reachable when the new pods mount.
- **Does not:** merge quarantined data back into the share. A human does
  that, following the runbook.
- **Tested by:** e2e `TestS4WrongFSType`, `TestS5StatefulSet`, `TestS6ShadowFiles`
  (state 3), `TestS13SambaNewIP` (state 2, with a new IP); sim `s04-wrong-fstype`,
  `s05-statefulset`, `s06-shadow-globalmount`.

### IP change

- **Symptom:** the server comes back under the same DNS name with a new IP.
  The kernel CIFS client keeps talking to the old address.
- **Detected as:** `stat_error` or `stat_timeout`. If the kernel re-resolves
  the name and reconnects by itself, nothing breaks and nothing happens.
- **Does:** a full Workload episode. Restarting `csi-smb-node` and re-staging
  resolves the name again, so the new mount uses the new address.
- **Outcome:** `healed`. Mounts break at slightly different times
  (`stat_timeout` is confirmed per mount), and an episode's scope is fixed by
  its trigger. So mounts that are confirmed while the first episode runs get
  the next episode on that node. No `SmbMount` triggers twice.
- **Does not:** change the PV, the server address or DNS. It relies on the CSI
  driver resolving the name again.
- **Tested by:** e2e `TestS13SambaNewIP`, sim `s13-samba-new-ip`.

### Expired or rotated credentials

> No e2e or sim scenario covers this. The behavior below follows from the
> state machine, not from a test
> ([#37](https://github.com/marcherschel/smb-mount-healer/issues/37)).

- **Symptom:** the password in the PV's `nodeStageSecretRef` Secret no longer
  matches the server. Nothing happens while the existing CIFS session lives.
  Once it reconnects, for example after a server restart or a network drop,
  the kernel fails to log in again and every `stat` returns an error.
- **Detected as:** typically `stat_error` (any errno from `stat`/`statfs`),
  `Broken` after the rechecks. If the reconnect hangs instead, `stat_timeout`.
- **Does:** a full Workload episode, because the healer can't tell an auth
  failure from any other dead session. The re-stage after the driver restart
  uses the same Secret, so it fails too, and the new pods stay in
  `ContainerCreating`.
- **Outcome:** `verify_failed` after `VERIFY_WAIT` (3m), and
  [`SmbMountHealerRemediationFailed`](runbooks/SmbMountHealerRemediationFailed.md)
  fires. Workloads are scaled back up but can't start. Each later trigger
  repeats this after the backoff (5m, 10m, … 1h), and more than two episodes
  per node in an hour raise
  [`SmbMountHealerRemediationLoop`](runbooks/SmbMountHealerRemediationLoop.md).
  The `csi-smb-node` logs (`-c smb`) on the node show the mount error.
- **Does not:** read, check or rotate credentials. Fix the Secret. If the
  mounts are already broken, the next episode after the backoff heals them, or
  you can scale and restart by hand as the runbooks describe.

### Partial outage: one share

- **Symptom:** one share or PV is broken; other shares on the same server or
  node are fine.
- **Does:** an episode whose scope is
  - the Deployments/StatefulSets owning the broken pods,
  - the other SMB PVs those pods mount (a two-volume pod is drained as one), and
  - *co-tenants*: other workloads on the node that use a scoped PV whose
    globalmount is not healthy. They must be drained too, or they keep the
    volume staged and it is never mounted again.
- **Does not:** touch PVs, workloads or nodes outside that scope. Host actions
  re-check live mountinfo just before they run (`onlyIf`), so a mount that is
  healthy by then is skipped.
- **Tested by:** e2e `TestS9CoTenantPinning`, `TestS11TwoVolumePod`; sim
  `s09-cotenant-pinning`, `s11-two-volume-pod`.

### Partial outage: server still down during the episode

- **Symptom:** the server is unreachable for longer than the episode.
- **Does:** everything up to `scaling_up` works without the server: drain,
  force-unmount, quarantine, driver restart. The new pods then can't mount.
- **Outcome:** `verify_failed`. Workloads are back at their original replica
  count but can't start until the server returns. Backoff keeps a second
  episode from starting while kubelet recovers the pods on its own once the
  server is back.
- **Does not:** keep retrying faster, or leave workloads at 0 waiting for the
  server.
- **Tested by:** e2e `TestS14OutageDuringEpisode`, sim `s14-outage-during-episode`.

### Partial outage: several nodes at once

- **Symptom:** the same share breaks on several nodes, and one Deployment has
  pods on all of them.
- **Does:** one remediation per node, but never two whose scopes overlap. The
  second waits in `pending` (condition `ScopeConflict`). Scaling is cluster-wide,
  so the first episode scales the shared Deployment down on *every* node. The
  waiting episode may then find no broken pod of its own and end
  `skipped_no_owner` or as superseded. Nodes are healed one after another.
- **Tested by:** sim `x01-two-node-scope-contention`. See also
  [RWX volumes shared across many nodes](#rwx-volumes-shared-across-many-nodes).

### Leftover data on a dead or orphaned path

- **Symptom:** no pod uses the PV on the node any more, but its globalmount is
  still mounted and dead, or a directory that should be empty has data
  (`globalmount_not_empty`, `orphan_pod_dir_not_empty`,
  `orphan_pod_dir_unhealthy`, `orphan_globalmount_broken`).
- **Does:** a *Sweep* remediation: force-unmount what's dead, quarantine what
  was written, wait for the unstage. It scales nothing.
- **Tested by:** e2e `TestS6ShadowFiles`, `TestS7OrphanPodDir`,
  `TestS8DeadOrphanGlobalmount`.

### A pod the healer can't scale pins the volume

- **Symptom:** a bare pod, Job, CronJob or DaemonSet pod uses the broken PV on
  the node.
- **Does:** drains what it can. The unowned pod keeps the volume staged, so
  `awaiting_unstage_completion` times out with a Warning event
  `UnstagePinned` that names the pod. If none of the broken pods has an
  owner, the episode ends `skipped_no_owner` right after planning.
- **Outcome:** `verify_failed`. The recreated pods get the same broken mount.
  Once the pinning pod is deleted, the next episode heals.
- **Does not:** delete, evict or restart pods it doesn't own through a
  Deployment/StatefulSet.
- **Tested by:** e2e `TestS15UnownedCoTenant`.

### Something scales the workload back up

- **Symptom:** a GitOps controller (Flux drift detection, Argo CD self-heal),
  an HPA or another operator writes `spec.replicas` back during the episode.
- **Does:** re-asserts `Scale(0)` (`scale_reassert`) in every stage up to
  the driver restart.
- **Outcome:** `healed` if the fighter reconciles on an interval. A controller
  that reverts every few seconds can win. Then draining hits
  `SCALE_DOWN_WAIT` (Warning `DrainTimeout`), and the mount may never be
  released. See the
  [RemediationLoop runbook](runbooks/SmbMountHealerRemediationLoop.md) for the
  Flux `driftDetection.ignore` fix.
- **Tested by:** e2e `TestS10FluxFighter` (an interval-based fighter only).

### The healer itself crashes

- **Does:** state is written to the `SmbMountRemediation` before each action,
  and every command is idempotent, so a restarted control plane, a new
  leader or a restarted worker picks the episode up where it stopped.
  Deleting an `smbr` rolls its workloads back to their original replicas
  through a finalizer.
- **Tested by:** e2e `TestS12ControlPlaneKilled`, `TestS12aWorkerKilled`,
  `TestS12bRemediationDeleted`; sim `cp-crash-*`, `w-restart-*`,
  `x02-leader-change`.

## What the healer never does

- **Delete data.** Files written to local disk are moved into the quarantine
  folder on the node and stay there. Merging them back is a human step
  ([runbook](runbooks/SmbMountHealerShadowDataPending.md)).
- **Mount anything.** It only unmounts. Mounting again is left to kubelet and
  the CSI driver.
- **Change configuration.** PVs, Secrets, credentials, mount options, DNS and
  the SMB server are never touched.
- **Touch pods it can't scale.** Bare pods, Jobs, CronJobs, DaemonSets and
  bare ReplicaSets are reported, never deleted or evicted.
- **Drain or cordon nodes, or evict pods.** It works only through
  `spec.replicas` of Deployments and StatefulSets, and restarts the node's
  `csi-smb-node` pod.
- **Act on uncertainty.** `Errored` mounts (`probe_error`) are not
  remediated, and every host action re-checks live mountinfo just before it
  runs.
- **Retry inside an episode.** A failed episode ends. The next one waits for
  the backoff.
- **Fix the server.** If the share is unreachable, the best outcome is
  `verify_failed` and an alert.

## When not to use this

The healer drains a node by scaling the **whole** owning Deployment or
StatefulSet to 0, through its scale subresource, and restores the original
replica count later. Scaling isn't per node, PodDisruptionBudgets don't
apply to it, and there is **no per-workload opt-out**
([#38](https://github.com/marcherschel/smb-mount-healer/issues/38)): every
Deployment/StatefulSet with a broken SMB pod on a node is in scope. The only
switches are cluster-wide [dry run](../README.md#safety) and aborting a
running episode. Think twice in these cases:

### RWX volumes shared across many nodes

A `ReadWriteMany` share used by a Deployment with replicas on many nodes: one
node's broken mount scales **every** replica, on every node, to 0 until the
episode is done (typically a few minutes, at most `EPISODE_MAX_DURATION`). A
mount problem on one node becomes a full outage of the workload. Co-tenant
scoping makes it worse: other workloads that use the same share on that node
are drained too. If several nodes break at once, their episodes run one after
another ([several nodes at once](#partial-outage-several-nodes-at-once)).

A cordon-and-drain of the bad node is often the better fix here, and the
healer does not do it.

### Workloads that can't be scaled to 0

Workloads with zero-downtime requirements, singletons that are expensive to
restart, or workloads whose replicas another controller keeps re-asserting.
Every episode takes them fully down, and
[`verify_failed`](#partial-outage-server-still-down-during-the-episode)
episodes repeat that after each backoff. A controller that fights the
scale-down every few seconds keeps the mount pinned
([something scales the workload back up](#something-scales-the-workload-back-up)).

### StatefulSets with strict quorum

etcd, ZooKeeper, Kafka, Galera, Elasticsearch masters and the like. Scaling
the StatefulSet to 0 takes down the **whole** ensemble, not just the member on
the bad node, so quorum is lost for the episode. Scaling back up follows the
StatefulSet's `podManagementPolicy` (one by one with `OrderedReady`), which
can take longer than the members' own timeouts. Quorum systems usually
shouldn't keep their state on SMB in the first place.

### Pods without a Deployment or StatefulSet owner

Jobs, CronJobs, DaemonSets and bare pods that use the share can't be scaled.
They keep the volume staged, so episodes on that node end `verify_failed` or
`skipped_no_owner`
([pinned volume](#a-pod-the-healer-cant-scale-pins-the-volume)).

### When the mount isn't the problem

Expired credentials, a server that is down for a long time, a firewall or
network policy blocking SMB: remounting can't fix any of these. The healer
cycles the affected workloads through scale-down and scale-up after every
backoff (5m, doubling to 1h) until the cause is fixed. During a known,
longer outage, switch to dry run.
