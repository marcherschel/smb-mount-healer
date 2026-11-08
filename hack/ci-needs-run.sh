#!/usr/bin/env bash
# Reads changed file paths (one per line) on stdin and prints "true" if the CI
# suite (lint, unit/envtest, e2e) has to run for them, "false" if every file
# is irrelevant to it. Used by the `changes` job of .github/workflows/ci.yaml;
# an empty list prints "true" (unknown diff: run everything).
#
# Irrelevant to CI:
#   - Markdown, except the generated files CI checks for drift
#     (docs/state-machine.md, the chart README rendered by helm-docs);
#   - workflows that are not ci.yaml (renovate, releases, CI image build) and
#     the octo-sts trust policies under .github/chainguard/.
set -euo pipefail

ignored='(\.md$)|(^\.github/workflows/(renovate|release-app|release-chart|ci-image)\.yaml$)|(^\.github/chainguard/)'
generated='^(docs/state-machine\.md|production/smb-mount-healer/README\.md)$'

files=$(grep -v '^[[:space:]]*$' || true)
if [ -z "$files" ]; then
  echo true
  exit 0
fi
relevant=$(printf '%s\n' "$files" | grep -Ev "$ignored" || true)
generated_md=$(printf '%s\n' "$files" | grep -E "$generated" || true)
if [ -n "$relevant" ] || [ -n "$generated_md" ]; then
  echo true
else
  echo false
fi
