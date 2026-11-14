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

CI runs the same checks plus the e2e suite (`make e2e`) on every pull request,
again after the merge to `main` (a PR does not have to be up to date with
`main` before it merges, so this is where merged PRs are tested together), and
before every release. How much of it runs depends on the change
(`hack/ci-needs-run.sh`, pinned by `hack/ci-needs-run_test.sh`; skipped checks
count as passed):

- **nothing:** Markdown, images/demo media, workflows other than `ci.yaml`,
  the CI image sources, the devcontainer, release-only files (`Dockerfile`,
  `hack/dist-*.sh`, `hack/next-version.sh`, `hack/stamp-chart.sh`),
  `renovate.json`, `LICENSE`, `.gitignore`.
- **lint job only:** the chart (`production/smb-mount-healer/`), `monitoring/`,
  `.golangci.yaml`, the lint-only scripts and `tools.env` (chart lint/test,
  generated files, chart defaults).
- **full suite:** everything Go-exposed: Go code, `go.mod`/`go.sum`, `api/`,
  `config/`, testdata, `test/e2e/`, the `Makefile`, `Dockerfile.tilt`,
  `versions.env`, `ci.yaml`, and any file not listed above. You can run e2e
locally on Linux if the machine running Docker has the `cifs` kernel module
(`sudo modprobe cifs`); it takes about 25 minutes.

Changes to the remediation state machine (`internal/remediation/table.go`)
need `go generate ./internal/remediation` for `docs/state-machine.md` and
usually `make sim-update`. Review the golden trace diff: it shows exactly
which transitions changed.

## Releasing

The app (binary + image) and the chart are released independently, from
different tags in this repository. Nobody picks or bumps a version by hand:
both releases are started from the Actions tab (Run workflow, on `main`)
with the kind of release, and the workflow computes the version from the
existing tags (`hack/next-version.sh`):

| Input | Next version (example: latest stable 0.1.4) |
|---|---|
| `bump: patch` / `minor` / `major` | `0.1.5` / `0.2.0` / `1.0.0`; promotes an open release candidate of that version |
| ... with `prerelease` | `0.1.5-rc.1` / `0.2.0-rc.1` / `1.0.0-rc.1` (or the next `-rc.N` of that version) |
| `bump: rc` | the next candidate of the open release candidate line (`0.2.0-rc.1` -> `0.2.0-rc.2`); fails if none is open |
| `tag: <existing tag>` | releases that existing tag again (for example after a failed run); `bump` is ignored |

| | App | Chart |
|---|---|---|
| Tag | `vX.Y.Z` | `chart-vA.B.C` |
| Workflow | `.github/workflows/release-app.yaml` | `.github/workflows/release-chart.yaml` |
| Tag points at | the `main` commit, created once the CI suite passed on it | a release commit on top of `main` with the stamped `Chart.yaml` (reachable only from the tag) |
| Publishes | `ghcr.io/marcherschel/smb-mount-healer:X.Y.Z`; GitHub release with static binaries, CRDs, `SHA256SUMS` (`make dist-app`) | `oci://ghcr.io/marcherschel/charts/smb-mount-healer` A.B.C; GitHub release with chart package, dashboard, rules, PrometheusRule, `SHA256SUMS` (`make dist-chart`) |

The app release runs the full CI suite first (unless skipped, see below);
the chart release runs only the chart checks. Only versions with a
pre-release suffix (`-rc.1`, ...) are marked as prereleases. App releases
become the repository's "Latest" release on GitHub (they carry the binaries
and CRDs); chart releases never do, since the chart is installed from the
OCI registry. Both workflows sign what they publish with cosign (keyless,
GitHub OIDC).

### App release

1. Run `release-app` on `main` with `bump` (and `prerelease` for a release
   candidate). It runs the CI suite on the `main` commit; only when it
   passes does it create the tag (so a failed run leaves no tag, and the
   next run computes the same version) and publish. `skip_tests` publishes
   without running the suite first (the commit already passed it in its
   PR); the GitHub release notes say so. Pushing a tag yourself
   (`git tag -s v0.2.0 -m v0.2.0 && git push origin v0.2.0`, with
   `[skip tests]` in the message to skip the suite) still works.
2. Renovate then moves the chart's `appVersion` (and the
   `artifacthub.io/images` tag) on `main` to the new release. The next chart
   release pins the latest stable app release either way.

### Chart release

`Chart.yaml` `version` on `main` is a placeholder (`0.0.0-dev`) and is never
bumped in PRs. Do add an `artifacthub.io/changes` entry with every chart
change (and set `artifacthub.io/containsSecurityUpdates: "true"` if the
release fixes a vulnerability); entries an earlier release already listed
are dropped automatically, so they never have to be cleaned up.

1. Run `release-chart` on `main` with `bump` (and `prerelease` for a
   release candidate).
2. The workflow stamps the release commit (`hack/stamp-chart.sh`):
   `version`, `appVersion` pinned to the latest stable app release (newest
   `vX.Y.Z` tag without suffix), the `artifacthub.io/images` tag,
   `artifacthub.io/prerelease`, and `artifacthub.io/changes` without the
   entries of the previous release (plus a "Default image" entry when
   `appVersion` moved). It refuses to release a chart identical to the
   previous release.
3. It runs the chart checks on that commit (chart lint/test, dashboard and
   alert metric names; the Go suite gates the app release, not the chart),
   tags it `chart-vA.B.C` and publishes. It refuses a version that is
   already published, or an `appVersion` whose image does not exist.

When a change needs both (for example a new setting in the binary and the
chart), release the app first and the chart after it.
