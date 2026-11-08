#!/usr/bin/env bash
# Reads changed file paths (one per line) on stdin and prints how much of the
# CI suite has to run for them. Used by the `changes` job of
# .github/workflows/ci.yaml.
#
#   full   lint, unit/envtest and e2e (any change not covered below; also an
#          empty list, since an unknown diff runs everything)
#   chart  only the lint job (chart lint/test, generated files, chart version
#          guard, chart defaults vs binary defaults): every relevant change is
#          in the chart (production/smb-mount-healer/) or its monitoring
#          sources (monitoring/)
#   none   nothing: only Markdown (except docs/state-machine.md, which CI checks
#          for drift against the code), images and demo media (docs/demo/,
#          *.png/.gif/.svg/.mp4), workflows other than ci.yaml, or the octo-sts
#          trust policies under .github/chainguard/
set -euo pipefail

ignored='(\.md$)|(\.(png|gif|svg|mp4)$)|(^docs/demo/)|(^\.github/workflows/(renovate|release-app|release-chart|ci-image)\.yaml$)|(^\.github/chainguard/)'
chart='^(production/smb-mount-healer/|monitoring/)'
always='^docs/state-machine\.md$'

files=$(grep -v '^[[:space:]]*$' || true)
if [ -z "$files" ]; then
  echo full
  exit 0
fi
# Chart files (including the generated chart README) never count as ignored.
relevant=$(printf '%s\n' "$files" | grep -E "$chart|$always" || true)
relevant+=$'\n'$(printf '%s\n' "$files" | grep -Ev "$chart|$always" | grep -Ev "$ignored" || true)
relevant=$(printf '%s\n' "$relevant" | grep -v '^$' || true)

if [ -z "$relevant" ]; then
  echo none
elif printf '%s\n' "$relevant" | grep -qvE "$chart"; then
  echo full
else
  echo chart
fi
