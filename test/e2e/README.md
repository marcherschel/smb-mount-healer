# smb-mount-healer e2e

End-to-end suite for the operator, run against a dedicated, disposable k3d
cluster (`smbmh-e2e`: 1 server + 1 agent, k3s `K3S_VERSION` from `versions.env`). The
suite is destructive (it unmounts globalmounts, freezes samba, kills pods), so
it never runs against any other cluster: the Tiltfile refuses any
context that is not `k3d-smbmh-e2e*` unless `SMBMH_E2E_ALLOW_CONTEXT` names it.

```
test/e2e/
  k3d.yaml                 cluster config (registry, traefik/metrics-server off, agent label)
  Tiltfile                 environment + suite runner (tilt ci / tilt up)
  hack/preflight.sh        cifs + mount-propagation checks inside the node containers
  manifests/samba/         samba (config-a, config-b, config-c, dl, library) + samba-landmine (landmine)
  manifests/fixtures/      node-stage Secret, StorageClass, static PVs/PVCs, heartbeat workloads, flux-fighter
  manifests/chaos/         privileged per-node DaemonSet for fault injection
  framework/               Go helpers (//go:build e2e)
  *_test.go                scenarios (//go:build e2e); S1 in baseline_test.go
```

## Requirements

- **cifs in the host kernel.** Mounts happen in the kernel of the machine that
  runs dockerd. The node containers cannot load modules from their own image;
  the kernel autoloads `cifs` on the first cifs mount if the module exists on
  the host. `hack/preflight.sh` checks `/proc/filesystems` and, if needed,
  triggers the autoload with a probe mount, then fails with a clear message.
  On the host/VM: `sudo modprobe cifs` (Ubuntu: `linux-modules-extra-$(uname -r)`).
  The devcontainer cannot `modprobe` (its `/lib/modules` does not match the
  VM kernel); load the module on the VM running Docker.
- **sch_netem** (optional): only the netem variant of S3 needs it; it is
  skipped when unavailable.
- **Mount propagation.** The worker, `csi-smb-node` and chaos pods mount
  `/var/lib/kubelet` with `mountPropagation: Bidirectional`, which needs `/` in
  the node container to be `rshared`. k3d >= v5.5 does this in
  `/bin/k3d-entrypoint-mounts.sh` (`mount --make-rshared /`), enabled by
  default (`K3D_FIX_MOUNTS` unset => on; verified in k3d v5.9.0
  `pkg/types/fixes/fixes.go`). Do not export `K3D_FIX_MOUNTS=0`. Preflight
  checks the `shared:` tag of the mount holding `/var/lib/kubelet` in each
  node's mountinfo.
- Tools: docker, k3d, tilt, kubectl, helm, yq, Go at the `versions.env` pins
  (`make tools` installs them; the devcontainer and the CI image
  `.github/ci-image` have them).
- Network egress: the chart repo (raw.githubusercontent.com), ghcr.io, Docker
  Hub, and the alpine package mirrors (the chaos pod installs cifs-utils and
  util-linux at start).

## Running locally

```sh
make e2e           # ephemeral: create a throwaway cluster (smbmh-e2e-run<ts>), run the
                   # suite once (e2e-run), ALWAYS delete the cluster; exits with the suite's result
```

`make e2e` leaves nothing behind: the cluster and its kubeconfig file are
removed on success, failure and Ctrl-C. Clusters never touch `~/.kube/config`;
each gets its own kubeconfig file (`~/.kube/k3d-<cluster>.yaml`, override with
`E2E_KUBECONFIG`), so the current kube context is never switched.

### Following a run

`make e2e`/`make e2e-run` tee both phases to `test/e2e/_runs/` (override with
`E2E_LOG_DIR`, git-ignored, uploaded as CI failure artifacts) and print the
paths at start and end, so a run can be followed by other people or agents and
inspected after the ephemeral cluster is gone:

```sh
tail -f test/e2e/_runs/<cluster>.tilt.log      # environment setup (tilt ci)
tail -f test/e2e/_runs/<cluster>.go-test.log   # the suite (go test -v)
grep '^=== PROGRESS' test/e2e/_runs/<cluster>.go-test.log | tail -1   # e.g. 15/20 (75%), passed/failed, elapsed
grep -E '^(--- FAIL|FAIL|ok )' test/e2e/_runs/<cluster>.go-test.log
```

The suite runs through `hack/go-test.sh`, which runs the top-level tests
selected by `-run`/`-skip` in `E2E_GO_TEST_FLAGS` one `go test` at a time in a
fixed order (version sort of the names: S1, S2, S3, S3…Netem, S4 … S12, S12a,
S12b, S13 …), each with its own timeout (`E2E_TEST_TIMEOUT`, default 20m), and
prints a `=== PROGRESS n/total (x%)` line after every test and the failed
names at the end. Regexes with `$` must not go through make (it expands them);
the test names are unique as substrings, e.g.
`E2E_GO_TEST_FLAGS="-run 'TestS4WrongFSType|TestS5StatefulSet'"`.

`<cluster>` is `smbmh-e2e-run<ts>` for `make e2e`. Under `make e2e-dev` the
`e2e-tests` resource writes the same `<cluster>.go-test.log`. The script exits
non-zero when any test failed (pipefail), so a failing suite still fails
`make e2e`, which still deletes its cluster.

For interactive debugging, keep a cluster around explicitly and delete it
when done:

```sh
make e2e-up        # k3d cluster smbmh-e2e (kubeconfig ~/.kube/k3d-smbmh-e2e.yaml)
make e2e-dev       # tilt up; trigger "e2e-tests" from the UI or `tilt trigger e2e-tests`
make e2e-run       # or: tilt ci brings the environment up (SMBMH_E2E_SETUP_ONLY=1), then
                   # go test runs the suite once (what CI runs)
make e2e-down      # delete the cluster and its kubeconfig file
```

Run a single scenario against a running environment:

```sh
KUBECONFIG=~/.kube/k3d-smbmh-e2e.yaml E2E_KUBE_CONTEXT=k3d-smbmh-e2e \
  go test -tags e2e ./test/e2e/... -p 1 -count=1 -run TestS1 -v
# or through make (setup + suite):
E2E_GO_TEST_FLAGS='-run TestS1' make e2e-run
```

Env knobs:

| Variable | Used by | Meaning |
|---|---|---|
| `E2E_KUBE_CONTEXT` | framework | kubeconfig context (default: current context). Tilt sets it. |
| `E2E_BASELINE_TIMEOUT` | framework | how long `WaitBaseline` waits (Go duration, default 10m) |
| `E2E_GO_TEST_FLAGS` | Tiltfile, `make e2e-run` | extra `go test` flags |
| `E2E_LOG_DIR` | Tiltfile, `make e2e-run` | where the `<cluster>.{tilt,go-test}.log` files go (default `test/e2e/_runs`) |
| `SMBMH_E2E_SETUP_ONLY=1` | Tiltfile | no `e2e-tests` resource (`make e2e-run` sets it and runs `go test` itself, because tilt ci would report the pods the suite kills on purpose as errors) |
| `SMBMH_E2E_IMAGE_MODE=k3d-import` | Tiltfile | `docker build` + `k3d image import` instead of pushing to the k3d registry |
| `SMBMH_E2E_CLUSTER` | Tiltfile | k3d cluster name if it cannot be derived from the context (`k3d-<name>`) |
| `SMBMH_E2E_ALLOW_CONTEXT` | Tiltfile | allow a context other than `k3d-smbmh-e2e*` |

## CI

The `e2e` job of `.github/workflows/ci.yaml` runs in the CI image (`.github/ci-image`) with the
Docker socket mounted:

```sh
E2E=smbmh-e2e-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT
make e2e-up E2E_CLUSTER=$E2E
E2E_CLUSTER=$E2E hack/ci-e2e.sh connect      # join the job container to the k3d network
E2E_CLUSTER=$E2E hack/ci-e2e.sh kubeconfig   # point the API server at k3d-$E2E-server-0:6443
SMBMH_E2E_IMAGE_MODE=k3d-import make e2e-run E2E_CLUSTER=$E2E
E2E_CLUSTER=$E2E hack/ci-e2e.sh cleanup; make e2e-down E2E_CLUSTER=$E2E   # if: always()
```

- The cluster name is the only per-run input; the registry (`k3d-<name>-registry`,
  random host port) and the kubeconfig context follow from it.
- The registry's host port is published on the Docker host, not on the job
  container's localhost, so CI uses `SMBMH_E2E_IMAGE_MODE=k3d-import`.
- After joining the k3d network, point the kubeconfig at the server
  container instead of the host-published `0.0.0.0:<port>` (k3d.yaml disables
  the load balancer): `hack/ci-e2e.sh kubeconfig` (the server cert carries this SAN via `--tls-san` in `make e2e-up`).
- The Tiltfile does not need python3 or tilt extensions (helm runs as a plain
  `local_resource`).
- On failure collect: `kubectl get smbm,smbr,smbt,smbq -o yaml`, events, logs of
  both healer modes and `csi-smb-node`, and each node's
  `docker exec <node> cat /proc/self/mountinfo`.

## What the Tiltfile deploys

In dependency order:

1. **preflight** (`hack/preflight.sh <cluster>`).
2. **csi-driver-smb**: `helm upgrade --install --wait` into `kube-system`,
   chart version and repository URL from `versions.env`
   (`CSI_DRIVER_SMB_VERSION`, `CSI_DRIVER_SMB_REPO`), default values plus
   `windows.enabled=false`. (Not `ext://helm_resource`: that extension needs
   python3 and a GitHub fetch at load time, which the CI image lacks.)
3. **samba / samba-landmine** in `smbmh-samba`:
   `ghcr.io/servercontainers/samba:smbd-only-a3.24.2-s4.23.8-r0` pinned by
   digest, configured through env (`ACCOUNT_e2esmbuser`, `UID_e2esmbuser=1000`,
   `SAMBA_VOLUME_CONFIG_*`), share data in a hostPath on the server node so it
   survives restarts. Headless Services: `//samba.smbmh-samba.svc.cluster.local/<share>`
   resolves to the pod IP.
4. **fixtures** in `smbmh-fixtures`, all pinned to the agent node
   (`e2e.smbhealer.herschel.io/fixtures=true`):
   - Secret `storagebox-e2e-credentials` in `kube-system`, StorageClass
     `storagebox-e2e`, static PVs `storagebox-e2e-<share>` (name == volumeHandle,
     `nodeStageSecretRef`, typical `mountOptions`, `Retain`), PVCs `<pv>-claim` bound
     by `volumeName` - the shape of a static csi-driver-smb setup for a hosted
     SMB share.
   - Heartbeat workloads (busybox, `hb-$POD_NAME` every 2s into every volume):

     | Workload | Kind | Replicas | Volumes |
     |---|---|---|---|
     | deploy-single | Deployment | 1 | config-a |
     | sts-single | StatefulSet | 1 | config-b |
     | two-vol | Deployment | 1 | config-c + dl |
     | cotenant-a, cotenant-b | Deployment | 1 each | library (shared) |
     | landmine-app | Deployment | 0 | landmine (on samba-landmine) |

   - **flux-fighter** (server node): a kubectl loop driven by ConfigMap
     `flux-fighter` (`enabled`, `target`, `replicas`, `interval`) that scales
     the target back whenever it drifts. Off by default.
5. **chaos** DaemonSet in `smbmh-chaos`: privileged, host network,
   `/var/lib/kubelet` Bidirectional, alpine + cifs-utils/util-linux.
6. **smb-mount-healer**: `docker_build('smb-mount-healer', Dockerfile.tilt)` +
   the Helm chart `production/smb-mount-healer` (release `smb-mount-healer`,
   values `values-e2e.yaml`: Tilt image ref, short timeouts) +
   `manifests/healer-namespace.yaml` (the chart does not create its namespace).
   Started after the fixtures are Running, so S1 sees a steady state.
7. **e2e-tests**: `go test -tags e2e ./test/e2e/... -p 1 -count=1 -timeout 50m`;
   manual under `tilt up`; `make e2e-run` skips it (`SMBMH_E2E_SETUP_ONLY=1`)
   and runs the same `go test` after `tilt ci` has brought the environment up.

## Framework

`framework.New(t)` returns an `*F` bound to the test (Gomega `WithT`, default
Eventually 2m/2s, Consistently 30s/2s) and registers `WaitBaseline` with
`t.Cleanup`. No sleeps: everything waits with `Eventually`/`Consistently`.

| Helper | What it does |
|---|---|
| `Metrics(component)` | scrapes `metrics.ComponentControlPlane` or `metrics.ComponentWorker` (worker on the agent node) through the API-server pod proxy, parsed with expfmt |
| `Delta(before, after)` | `.Value(name, Labels)` / `.Get(name, k, v, ...)`: after - before, summed over matching series (histograms: sample count) |
| `SmbMounts()`, `SmbMount(node, pv)`, `Remediations()`, `NewRemediations()`, `HostTasks()`, `HostTasksOf(smbr)`, `Quarantines()` | CR listings |
| `WaitRemediation(outcome)` | waits for a remediation created during the test to reach `done`; fails fast on another outcome |
| `StageTrace(name)` | stages from `status.transitions` |
| `Chaos.Exec(node, cmd...)`, `Chaos.Sh`, `MountInfo`, `LazyUnmount`, `MountCIFS`, `WriteFile` | host actions via client-go exec into the chaos pod |
| `Samba.Pause(d)`, `Resume()`, `Restart()`, `Scale(n)`, `Cat(share, file)`, `Ls(share)` | samba control (also `Landmine`) |
| `Fixtures.DetachGlobalmount(pv)`, `DeletePod(w)`, `Scale(w, n)`, `Replicas(w)`, `EnableFighter(target, n)`, `DisableFighter()`, `Heartbeat(w)`, `GlobalmountPath(pv)`, `PodVolumePath(uid, pv)` | fixture manipulation |
| `Kill(component)` | deletes the controlplane pod / the agent's worker pod and waits for the replacement |
| `WaitBaseline()` | samba ready, fighter off, every workload at its manifest replicas and ready, an `smbm` per staged PV and all Healthy, no non-terminal `smbr`, no `smbq` (unless `AllowQuarantines()`) |
| `RecordSmbMounts()` | watches every `smbm` for the test's lifetime: `Healths`, `Stages` (STAGE column), `SawHealth` (also pod mounts), `Describe` |
| `Events(kind, name, since)`, `EventReasons`, `AllEventsText` | events.k8s.io Events (any namespace), e.g. `Warning/MountBroken` |
| `StartBarePod(name, pvs...)`, `DeleteBarePod` | unowned pod on the agent node using the PVs (pinner / unowned co-tenant) |
| `Freeze(pod)`, `Thaw(pod)` | SIGSTOP/SIGCONT a pod's processes through the chaos pod (hostPID) |
| `HealerPod`, `KillNow(component)`, `PodLogs`, `SetControlPlaneDryRun(bool)` | healer pod helpers (`KillNow`: grace period 0) |
| `ClearQuarantines()` option, `RemoveQuarantineFolders()`, `QuarantineFolders()` | tests that quarantine data remove `QuarantineRoot/*` on the agent node before the baseline wait |

`New` deletes the terminal `smbr` left by earlier tests (logging their
trace): they are the backoff history, and back-to-back scenarios on the same
PV would otherwise be suppressed by backoff.

**WRONG_FSTYPE injection** (S4, S5, S9-S12b, S15, S16; `injectWrongFSType` in
`helpers_test.go`): a bare "pinner" pod keeps the PV in use while the
globalmount is detached and the workload pod recreated, and is deleted as
soon as the replacement pod got the bare directory (well within
`POD_STARTUP_GRACE`). Without it kubelet may unstage and stage the volume
cleanly in between (or retry NodeStage after a failed unstage), and nothing
breaks.

## Fault catalogue (plan Step 3) and test files

| # | Scenario | Injection (framework) | File |
|---|---|---|---|
| S1 | Baseline | - | `baseline_test.go` |
| S2 | Transient blip | `Samba.Pause(7s)` | `transient_test.go` |
| S3 | Slow server | `Samba.Pause(20s)` (< SLOW_CONFIRM_WINDOW 60s, which leaves room for the client's recovery tail after SIGCONT); `TestS3SlowServerNetem`: 6s netem delay on the samba veth (skipped without sch_netem) | `transient_test.go` |
| S4 | WRONG_FSTYPE | pinner + `DetachGlobalmount(config-a)` + `DeletePod(deploy-single)` | `workload_test.go` |
| S5 | StatefulSet | S4 on `sts-single` | `workload_test.go` |
| S6 | Shadow files in an unmounted globalmount | detach gm, `Chaos.WriteFile` into it, `Scale(w, 0)` | `sweep_test.go` (`AllowQuarantines`) |
| S7 | Orphaned pod dir | `Chaos.WriteFile(pods/<rand-uid>/volumes/kubernetes.io~csi/<pv>/mount/old.txt)` | `sweep_test.go` |
| S8 | Dead orphan globalmount | `Chaos.MountCIFS(//samba-landmine.../landmine, GlobalmountPath(storagebox-e2e-landmine))`, `Landmine.Scale(0)` | `sweep_test.go` |
| S9 | Co-tenant pinning | `DetachGlobalmount(library)` + `DeletePod(cotenant-b)` | `cotenant_test.go` |
| S10 | Flux fighter | S9 + `EnableFighter("deployment/cotenant-a", 1)` | `cotenant_test.go` |
| S11 | Two-volume pod | `DetachGlobalmount(dl)` on `two-vol` | `workload_test.go` |
| S12 | Control plane killed mid-remediation | shadow file; worker frozen in draining, then the csi-smb `smb` container frozen (kubelet cannot unstage) and the worker thawed; `Kill(controlplane)` in `awaiting_unstage_completion` | `resume_test.go` |
| S12a | Worker killed mid-host-task | worker frozen in `draining`, `KillNow(worker)` with the `releasing_mounts` task pending (a quarantine task runs in milliseconds and cannot be interrupted deterministically) | `resume_test.go` |
| S12b | Remediation deleted mid-episode | delete the `smbr` in `draining` | `resume_test.go` |
| S13 | Samba restart with a new IP | `Samba.Restart()` | `samba_test.go` |
| S14 | Outage during an episode | `Samba.Scale(0)` for 4m | `samba_test.go` |
| S15 | Unowned co-tenant | bare Pod on library + S9 | `cotenant_test.go` |
| S16 | Dry run | control plane `--dry-run` (patch the Deployment) + S4 | `dryrun_test.go` |
| S17 | Quarantine lifecycle | after S6: chaos edits under `framework.QuarantineRoot` | `quarantine_test.go` |
