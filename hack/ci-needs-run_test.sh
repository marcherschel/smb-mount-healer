#!/usr/bin/env bash
# Table test for hack/ci-needs-run.sh (run by the lint job):
#
#   hack/ci-needs-run_test.sh
#
# Each case is "<expected suite> <path>[,<path>...]". It also checks that
# every tracked file classifies without error, and that no tracked file
# outside the documented full-suite areas runs the full suite.
set -euo pipefail

cd "$(dirname "$0")/.."
run=hack/ci-needs-run.sh
fail=0

check() {
  local want=$1 paths=$2 got
  got=$(tr ',' '\n' <<<"${paths}" | "${run}")
  if [ "${got}" != "${want}" ]; then
    echo "FAIL ${paths}: got ${got}, want ${want}"
    fail=1
  fi
}

# full: Go code and everything that changes what the tests run against.
check full internal/health/step.go
check full go.mod
check full go.sum
check full api/v1alpha1/smbmount_types.go
check full config/crd/bases/smbhealer.herschel.io_smbmounts.yaml
check full internal/sim/testdata/traces/s01-baseline.golden
check full production/defaults_test.go
check full test/e2e/values-e2e.yaml
check full test/e2e/Tiltfile
check full test/e2e/hack/go-test.sh
check full test/e2e/manifests/samba/samba.yaml
check full hack/ci-e2e.sh
check full hack/ci-needs-run.sh
check full Makefile
check full Dockerfile.tilt
check full .dockerignore
check full versions.env
check full .github/workflows/ci.yaml
check full docs/state-machine.md
check full some/new/file.txt
check full ""
check full README.md,internal/health/step.go
check full production/smb-mount-healer/values.yaml,cmd/smb-mount-healer/main.go
check full tools.env,versions.env

# lint: chart, monitoring, lint config, lint-only scripts, tool pins.
check lint production/smb-mount-healer/values.yaml
check lint production/smb-mount-healer/README.md
check lint monitoring/dashboards/smb-mount-healer.json
check lint .golangci.yaml
check lint tools.env
check lint hack/chart-test.sh
check lint hack/check-dashboard-metrics.sh
check lint hack/sync-chart.sh
check lint hack/ci-needs-run_test.sh
check lint tools.env,docs/development.md

# none: docs, media, other workflows, CI image, devcontainer, release-only files.
check none README.md
check none docs/runbooks/SmbMountHealerRemediationFailed.md
check none docs/demo/realistic-env.sh
check none docs/logo.svg
check none .github/workflows/renovate.yaml
check none .github/workflows/release-app.yaml
check none .github/chainguard/renovate.sts.yaml
check none .github/ci-image/Dockerfile
check none .github/ci-image/build.sh
check none .devcontainer/devcontainer.json
check none hack/tools/install.sh
check none Dockerfile
check none hack/dist-app.sh
check none hack/next-version.sh
check none hack/stamp-chart.sh
check none LICENSE
check none .gitignore
check none renovate.json
check none renovate.json,.devcontainer/devcontainer.json,README.md

# Every tracked file classifies; full only for the documented areas.
full_ok='(\.go$)|(^go\.(mod|sum)$)|(^api/)|(^cmd/)|(^internal/)|(^config/)|(^test/e2e/)'
full_ok+='|(^Makefile$)|(^Dockerfile\.tilt$)|(^\.dockerignore$)|(^versions\.env$)'
full_ok+='|(^\.github/workflows/ci\.yaml$)|(^hack/ci-(e2e|needs-run)\.sh$)|(^docs/state-machine\.md$)'
while IFS= read -r f; do
  got=$(printf '%s\n' "${f}" | "${run}")
  if [ "${got}" = full ] && ! grep -qE "${full_ok}" <<<"${f}"; then
    echo "FAIL ${f}: runs the full suite but is not Go-exposed (classify it in ${run}, or extend full_ok here)"
    fail=1
  fi
done < <(git ls-files)

if [ "${fail}" -ne 0 ]; then
  exit 1
fi
echo "ci-needs-run: ok"
