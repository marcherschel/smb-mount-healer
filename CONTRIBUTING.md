# Contributing

Issues and pull requests are welcome. For larger changes, please open an issue
first so we can agree on the approach.

## Development

See [docs/development.md](docs/development.md) for the setup, make targets
and test layers. Before you open a pull request:

```sh
make tidy-check lint generate manifests chart-docs   # commit anything this changes
make test test-envtest
make chart-lint chart-test
```

CI runs the same checks plus the e2e suite (`make e2e`). You can run e2e
locally on Linux if the machine running Docker has the `cifs` kernel module
(`sudo modprobe cifs`); it takes about 25 minutes.

Changes to the remediation state machine (`internal/remediation/table.go`)
need `go generate ./internal/remediation` for `docs/state-machine.md` and
usually `make sim-update`. Review the golden trace diff: it shows exactly
which transitions changed.

## Releasing

The app (binary + image) and the chart are released independently, from
different tags in this repository:

| | App | Chart |
|---|---|---|
| Tag | `vX.Y.Z` | `chart-vA.B.C` |
| Workflow | `.github/workflows/release-app.yaml` | `.github/workflows/release-chart.yaml` |
| Version source | the tag | `version` in `production/smb-mount-healer/Chart.yaml` (the tag must match) |
| Publishes | `ghcr.io/marcherschel/smb-mount-healer:X.Y.Z`; GitHub release with static binaries, CRDs, `SHA256SUMS` (`make dist-app`) | `oci://ghcr.io/marcherschel/charts/smb-mount-healer` A.B.C; GitHub release with chart package, dashboard, rules, PrometheusRule, `SHA256SUMS` (`make dist-chart`) |

Both workflows run the full CI suite first. `0.x` and `-rc` versions are
marked as prereleases.

### App release

1. Tag the commit on `main`: `git tag -s v0.2.0 -m v0.2.0 && git push origin v0.2.0`
   (`v0.2.0-rc.1` for a release candidate).
2. Renovate then opens a PR that moves the chart's `appVersion` (and the
   `artifacthub.io/images` tag) to the new release, bumps the chart
   `version` by a patch and adds a `changed` entry to
   `artifacthub.io/changes` (`hack/bump-chart-version.sh`). Merge it, then
   release the chart.

### Chart release

Every change below `production/smb-mount-healer/` needs a higher chart
`version` than the last `chart-v*` tag; CI enforces this
(`hack/check-chart-version.sh`). Bump it in your PR with
`hack/bump-chart-version.sh patch|minor|major`.

1. Curate `artifacthub.io/changes` in `Chart.yaml` for this version (only
   the changes since the last chart release). Set
   `artifacthub.io/containsSecurityUpdates: "true"` if the release fixes a
   vulnerability. Merge to `main`.
2. Tag: `git tag -s chart-v0.2.0 -m chart-v0.2.0 && git push origin chart-v0.2.0`.
3. The workflow refuses the tag if it differs from `Chart.yaml` `version`,
   if that version is already published, or if the image for `appVersion`
   does not exist.

When a change needs both (for example a new setting in the binary and the
chart), release the app first and the chart after it.
