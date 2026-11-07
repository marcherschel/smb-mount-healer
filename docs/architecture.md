# Architecture

This page describes how smb-mount-healer is built. For installing and
operating it, start with the [README](../README.md); the remediation stages
are specified in [state-machine.md](state-machine.md).

## The problem

[csi-driver-smb](https://github.com/kubernetes-csi/csi-driver-smb) mounts an
SMB share once per node and PV (the *globalmount*) and bind-mounts it into
every pod that uses the PV. When the SMB server goes away or the CIFS session
breaks, Kubernetes does not notice: the pods stay `Running` while their
volume

- hangs or returns errors (`Host is down`, `stat` timeouts), or
- is silently replaced by the empty directory underneath a mount that
  vanished, so the app writes to the **node's local disk** and its data never
  reaches the share.

The kubelet never re-stages a globalmount on its own while pods use it, so
recovering takes manual work: drain every pod of the PV on that node, unmount,
make the CSI driver stage the share again, scale back up, and rescue anything
written to local disk in the meantime. smb-mount-healer automates exactly
that, one node at a time, and refuses to delete data.

## Run modes

The same binary runs in one of two modes. All state lives in custom resources;
the healer writes nothing to disk except quarantined user data, which it moves
and never deletes.

| Mode | Runs as | Job |
|---|---|---|
| `--controlplane [--dry-run]` | Deployment, 1 replica, leader election | Only decision maker: creates `SmbMountRemediation`s, runs the remediation state machine, scales workloads, restarts the CSI node driver, creates `SmbHostTask`s. Never touches a host. |
| `--worker` | privileged DaemonSet, hostPath `/var/lib/kubelet` (Bidirectional) | Probes the node's mounts, publishes `SmbMount` health, runs `SmbHostTask` actions (force-unmount, quarantine, probe), keeps `SmbQuarantine`s in sync with the disk. Makes no remediation decisions. |
| `--version` | | Prints `smb-mount-healer <version> (<revision>, <go>)`. |

Pass exactly one of `--controlplane` and `--worker`, otherwise the process
exits with code 2 and prints usage. A hidden `--probe <op> <path>` mode is the
child process the worker spawns for `stat`, `statfs` and `umount`, so a hung
CIFS server can never block the main process.

### How a broken mount is healed

1. The **worker** on every node probes each SMB globalmount and pod mount
   (mount table, `stat`/`statfs` in a child process with a timeout) and
   publishes the result as an `SmbMount` per (node, PV). A per-mount debounce
   (`Healthy` -> `Suspect` -> `Broken`, with rechecks) filters out blips.
2. The **control plane** watches `SmbMount`s. A `Broken` mount (or leftover
   data on a dead orphan path) creates an `SmbMountRemediation` for that node,
   unless backoff or a conflicting remediation suppresses it.
3. The remediation walks its state machine ([state-machine.md](state-machine.md)):
   plan the scope (owners of the broken pods, sibling PVs, co-tenants of the
   same share), scale those Deployments/StatefulSets to 0, force-unmount dead
   mounts, move data written to local disk into a quarantine folder, restart
   the node's `csi-smb-node` pod, scale back up and verify that the new pods
   see a healthy mount.
4. Host actions (unmount, quarantine, probe) are never run by the control
   plane: it writes `SmbHostTask`s, and the worker on that node executes them
   and reports a result per action.
5. Quarantined data is reported as `SmbQuarantine` objects (counts and sizes
   only) and stays on the node until a human merges it back.

## Custom resources (`smbhealer.herschel.io/v1alpha1`, cluster-scoped)

| Kind | Short | One per | Written by | `kubectl get` columns |
|---|---|---|---|---|
| `SmbMount` | `smbm` | (node, PV) | spec + health status: worker; remediation status: control plane (SSA field managers) | NODE, PV, HEALTH, REASON, STAGE, LAST OUTCOME, AGE |
| `SmbMountRemediation` | `smbr` | remediation episode | control plane | NODE, TYPE, STAGE, OUTCOME, AGE (`-o wide`: DRY-RUN) |
| `SmbHostTask` | `smbt` | batch of host actions | spec: control plane; status: worker | NODE, PHASE, DRY-RUN, AGE |
| `SmbQuarantine` | `smbq` | quarantine folder on a node | worker | NODE, PV, FILES, SIZE (bytes), QUARANTINED, LAST SCAN |

`kubectl get smbhealer` lists all four kinds. All specs are immutable (CEL).
Constants live in `api/v1alpha1/constants.go`:

- finalizer `smbhealer.herschel.io/restore-replicas`
- annotation `smbhealer.herschel.io/abort=true`
- labels `smbhealer.herschel.io/{node,pv,remediation}`
- field managers `smb-mount-healer-{worker,controlplane}`
- condition types

`SmbQuarantine.status` holds only counts, bytes, times, `sizeTruncated` and
conditions. No file or directory name from quarantined data may appear in any
CR, Event, metric label or log line. `api/v1alpha1/privacy_test.go` enforces
this for the status type.

Typed server-side-apply configurations are generated into
`api/v1alpha1/applyconfiguration/api/v1alpha1` (package alias `ac` in the
envtest test). Use them with `c.Status().Apply(ctx, ac.SmbMount(name).WithStatus(...), client.FieldOwner(v1alpha1.FieldManagerWorker), client.ForceOwnership)`.
Every top-level status field is optional, so each manager only applies, and
only owns, its own fields.

## Layout and package boundaries

```
cmd/smb-mount-healer/      flag parsing -> controlplane.Run | worker.Run | probe.RunChild
api/v1alpha1/              CRD types, enums, constants, name helpers (+ generated deepcopy, applyconfiguration)
config/crd, config/rbac    generated by controller-gen (make manifests); inputs of hack/sync-chart.sh
production/smb-mount-healer  Helm chart: the one way to deploy (e2e and prod); see its README.md
production/defaults_test.go  chart values.yaml defaults == internal/config defaults
internal/config            flags + env per mode, validation
internal/kubelet           kubelet path math (globalmount = sha256(volumeHandle), pod dirs, quarantine names)
internal/hostfs            PURE interfaces/types: MountTable, DirInspector, Prober, Mutator, QuarantineInspector
internal/hostfs/linux      real implementation (worker only)
internal/hostfs/fake       in-memory fake (tests, sim)
internal/probe             self-exec child runner (stat/statfs/umount with timeout, hung tracking)
internal/inventory         PURE: PVs + pods + mountinfo + dir facts -> NodeView
internal/health            PURE: per-mount debounce FSM, rehydrated from SmbMount.status
internal/remediation       PURE: remediation FSM Step + Trigger (backoff, scope conflicts)
internal/command           PURE: typed idempotent commands + ClusterExecutor/HostExecutor interfaces
internal/kube              Cluster interface (owner resolution, scale, driver pods, events), NewScheme
internal/controlplane      controllers + cluster executor; RBAC markers (rbac.go)
internal/worker            runnables + host executor; RBAC markers (rbac.go)
internal/metrics           metric name/label contract (names.go) + collectors
internal/sim               simulated world + golden traces
test/e2e                   //go:build e2e suite (k3d + tilt)
```

`.golangci.yaml` enforces purity in `health`, `remediation`, `inventory`,
`command` and the root `hostfs` package:

- **forbidigo** forbids `time.Now`, `time.Since`, `time.Sleep` and timers, `os.*`, and `math/rand`.
- **depguard** forbids importing controller-runtime, client-go, `os`, `os/exec`, `net`, `hostfs/linux`, `probe` and `kube`.

Time is always an input. `exhaustive` requires every switch on a state enum
to list all cases; a `default` branch does not count.

## Metrics

Every metric is prefixed `smb_mount_healer_` and carries
`component=worker|controlplane`. Worker metrics also carry `node`; per-node
control-plane metrics use `target_node`. `namespace` and `pod` are never used
as label names, because Prometheus renames such self-exported labels to
`exported_*` unless `honor_labels` is set. The full contract
is in `internal/metrics/names.go`.

Build the collectors once in `Run`, then register them on controller-runtime's
registry:

```go
m := metrics.NewWorker(nodeName) // or metrics.NewControlPlane()
if err := m.Register(ctrlmetrics.Registry); err != nil { return err }
if err := metrics.RegisterBuildInfo(ctrlmetrics.Registry, metrics.ComponentWorker, version, revision); err != nil { return err }
```
