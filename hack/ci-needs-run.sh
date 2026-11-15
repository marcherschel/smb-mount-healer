#!/usr/bin/env bash
# Reads changed file paths (one per line) on stdin and prints how much of the
# CI suite has to run for them. Used by the `changes` job of
# .github/workflows/ci.yaml; hack/ci-needs-run_test.sh pins the decisions.
#
#   full  lint, unit/envtest/simulator and e2e: the change can alter what the
#         Go code does or what the tests run against. Everything not listed
#         below (Go code, go.mod/go.sum, api/, config/, testdata, test/e2e/,
#         Makefile, Dockerfile.tilt, .dockerignore, versions.env, ci.yaml, this
#         script, docs/state-machine.md, which a Go test checks against the
#         code), and also an empty list, since an unknown diff runs everything.
#   lint  only the lint job (golangci-lint, generated files, chart lint/test,
#         dashboard metric names, chart defaults vs binary defaults): the chart
#         (production/smb-mount-healer/) and its monitoring sources
#         (monitoring/), the lint config, the scripts only the lint job runs
#         (including hack/ci-needs-run_test.sh),
#         and tools.env (golangci-lint, helm and helm-docs pins).
#   none  nothing: Markdown (except docs/state-machine.md), images and demo
#         media, workflows other than ci.yaml, the octo-sts trust policies,
#         the CI image sources (ci.yaml pins the image by digest), the
#         devcontainer and its installer, the release-only image and scripts
#         (the release workflows run the full suite before they publish), and
#         repository metadata (LICENSE, .gitignore, renovate.json).
set -euo pipefail

ignored='(\.md$)|(\.(png|gif|svg|mp4)$)|(^docs/demo/)'
ignored+='|(^\.github/workflows/(renovate|release-app|release-chart|ci-image)\.yaml$)|(^\.github/chainguard/)'
ignored+='|(^\.github/ci-image/)|(^\.devcontainer/)|(^hack/tools/install\.sh$)'
ignored+='|(^Dockerfile$)|(^hack/(dist-app|dist-chart|next-version|stamp-chart)\.sh$)'
ignored+='|(^LICENSE$)|(^\.gitignore$)|(^renovate\.json$)'
lint='(^production/smb-mount-healer/)|(^monitoring/)|(^\.golangci\.yaml$)|(^tools\.env$)'
lint+='|(^hack/(chart-test|check-dashboard-metrics|sync-chart|ci-needs-run_test)\.sh$)'
always='^docs/state-machine\.md$'

files=$(grep -v '^[[:space:]]*$' || true)
if [ -z "$files" ]; then
  echo full
  exit 0
fi
# Lint-only files (including the generated chart README) and the checked
# state machine doc never count as ignored.
relevant=$(printf '%s\n' "$files" | grep -E "$lint|$always" || true)
relevant+=$'\n'$(printf '%s\n' "$files" | grep -Ev "$lint|$always" | grep -Ev "$ignored" || true)
relevant=$(printf '%s\n' "$relevant" | grep -v '^$' || true)

if [ -z "$relevant" ]; then
  echo none
elif printf '%s\n' "$relevant" | grep -qvE "$lint"; then
  echo full
else
  echo lint
fi
