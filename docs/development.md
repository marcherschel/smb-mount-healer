# Development

## Setup

The [devcontainer](../.devcontainer/devcontainer.json) has everything: Go,
Docker (for k3d and image builds) and the pinned tools. Outside of it you need
Go (the `toolchain` line in `go.mod`), Docker with buildx, and

```sh
make tools    # kubectl, helm, k3d, tilt, golangci-lint, yq, oras -> ~/.local/bin (sha256-verified)
```

Every tool and dependency version lives in [`versions.env`](../versions.env)
(plain `KEY=value`, read by make, the scripts, the CI image and the Tiltfile;
updated by Renovate). `controller-gen` and `setup-envtest` are pinned through
`tool` directives in `go.mod`. `make lint` and the chart targets download the
pinned golangci-lint and helm into `bin/` when the installed version differs.

## Make targets

| Target | What it does |
|---|---|
| `fmt` | gofmt + goimports via golangci-lint |
| `lint` | golangci-lint `GOLANGCI_LINT_VERSION`. Defaults to `--concurrency=1 GOGC=25` so it fits a 4 GiB machine; override with `make lint LINT_FLAGS= LINT_ENV=`. |
| `tidy-check` | `go mod tidy -diff` |
| `generate` | deepcopy + apply configurations (controller-gen via `go tool`) |
| `manifests` | CRDs into `config/crd/bases`; RBAC into `config/rbac/role-{controlplane,worker}.yaml`; then `hack/sync-chart.sh` regenerates the chart's CRD and ClusterRole templates and copies `monitoring/` into the chart's `files/` |
| `chart-docs` | chart `README.md` from `README.md.gotmpl` + `values.yaml` (helm-docs `HELM_DOCS_VERSION` via `go run`) |
| `chart-lint` | `helm lint --strict` with every `ci/*-values.yaml` |
| `chart-test` | `helm template` every `ci/*-values.yaml`, kubeconform and promtool (pinned images via docker), yq assertions, schema rejects bad values (`hack/chart-test.sh`) |
| `test` | `go test -race ./...` |
| `test-envtest` | envtest-tagged tests against kube-apiserver `KUBERNETES_VERSION` (assets in `bin/k8s` via `go tool setup-envtest`) |
| `sim-update` | regenerate the `internal/sim` golden traces |
| `build` | `bin/smb-mount-healer` |
| `image` | `docker buildx` of `$(IMG)` (default `ghcr.io/marcherschel/smb-mount-healer:$(VERSION)`). Native single-arch `--load` by default; multi-arch push: `PLATFORMS=linux/amd64,linux/arm64 IMAGE_OUTPUT=--push`. |
| `dist-app` | app release assets into `dist/app` for `VERSION`: static binaries (`RELEASE_PLATFORMS`), CRDs, `SHA256SUMS` (`hack/dist-app.sh`) |
| `dist-chart` | chart release assets into `dist/chart`: package as committed, dashboard, rules, PrometheusRule, `SHA256SUMS` (`hack/dist-chart.sh`) |
| `check-chart-version` | fail if the chart changed since the last `chart-v*` tag without a higher `Chart.yaml` version |
| `e2e` | the e2e suite on a throwaway k3d cluster, always deleted afterwards |
| `e2e-up` / `e2e-run` / `e2e-dev` / `e2e-down` | k3d cluster `smbmh-e2e` (`rancher/k3s:K3S_VERSION`); `e2e-run` = `tilt ci` environment setup on `test/e2e/Tiltfile` (healer via the Helm chart) + `go test` suite; `e2e-dev` = `tilt up` |
| `tools` | install the pinned CLI tools (`hack/tools/install.sh`) |

`VERSION` (the app version) defaults to `git describe --tags --match 'v[0-9]*'`
without the leading `v` (or `dev`); chart tags (`chart-v*`) never count.
Releases are described in [CONTRIBUTING.md](../CONTRIBUTING.md#releasing). CI fails when `make generate manifests chart-docs` produces a diff,
so commit generated files.

## Tests

- **Unit** (`make test`): pure packages, the simulator and the chart defaults
  test (`production/defaults_test.go`: `values.yaml` defaults == the
  binary's defaults).
- **envtest** (`make test-envtest`): controllers against a real
  kube-apiserver and etcd.
- **Simulator** (`internal/sim`): deterministic scenarios with golden traces
  (`make sim-update` rewrites them; review the diff).
- **Chart** (`make chart-lint chart-test`).
- **e2e** (`make e2e`, ~25 minutes): k3d + Tilt with samba servers, the real
  csi-driver-smb and fault injection. It needs the `cifs` kernel module on the
  machine that runs Docker. See [test/e2e/README.md](../test/e2e/README.md).

## Generated files

| Source | Generated | By |
|---|---|---|
| `api/v1alpha1/*_types.go` | `zz_generated.deepcopy.go`, `applyconfiguration/`, `config/crd/bases/` | `make generate manifests` |
| `+kubebuilder:rbac` markers in `internal/{controlplane,worker}/rbac.go` | `config/rbac/role-*.yaml` | `make manifests` |
| `config/` | chart `templates/crds/`, `templates/rbac/clusterrole-*.yaml` | `hack/sync-chart.sh` (`make manifests`) |
| `monitoring/` | chart `files/` | `hack/sync-chart.sh` (`make manifests`) |
| `internal/remediation/table.go` | `docs/state-machine.md` | `go generate ./internal/remediation` |
| chart `values.yaml` + `README.md.gotmpl` | chart `README.md` | `make chart-docs` |

## Monitoring sources

`monitoring/dashboards/smb-mount-healer.json` (Grafana) and
`monitoring/alerts/smb-mount-healer.rules.yaml` (Prometheus rule file) are the
sources of truth. Edit them there, run `make manifests`, and keep
`hack/check-dashboard-metrics.sh` green: it fails if they use a metric name
that `internal/metrics/names.go` does not define. Every alert needs a runbook
in `docs/runbooks/<alert>.md` (checked by `make chart-test`).
