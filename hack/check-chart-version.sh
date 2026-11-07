#!/usr/bin/env bash
# Fails if the chart changed since the latest chart release tag (chart-v*)
# while Chart.yaml `version` is not higher than that release, so chart changes
# can never pile up under an already published version. Run in CI (lint job)
# and by `make check-chart-version`; needs the git tags (fetch-depth: 0).
#
# "The chart" is production/smb-mount-healer/ (templates, values, CRDs, and
# files/ with the dashboard and alerts copied from monitoring/).
set -euo pipefail

cd "$(dirname "$0")/.."

CHART=production/smb-mount-healer
# versionsort.suffix=-: a prerelease (chart-v1.0.0-rc.1) sorts before its release.
latest=$(git -c versionsort.suffix=- tag -l 'chart-v*' --sort=-v:refname | head -1)
current=$(sed -n 's/^version:[[:space:]]*"\{0,1\}\([^"[:space:]]*\)"\{0,1\}.*/\1/p' "$CHART/Chart.yaml")

if [ -z "$latest" ]; then
  echo "check-chart-version: no chart release yet; Chart.yaml version $current"
  exit 0
fi
released=${latest#chart-v}

if git diff --quiet "$latest" -- "$CHART"; then
  echo "check-chart-version: chart unchanged since $latest"
  exit 0
fi

# semver_gt A B: true if A > B by SemVer precedence (1.0.0 > 1.0.0-rc.2 > 1.0.0-rc.1).
semver_gt() {
  local a=$1 b=$2 am=${1%%-*} bm=${2%%-*} ap= bp= i x y
  [[ $a == *-* ]] && ap=${a#*-}
  [[ $b == *-* ]] && bp=${b#*-}
  IFS=. read -ra x <<<"$am"
  IFS=. read -ra y <<<"$bm"
  for i in 0 1 2; do
    if ((10#${x[i]:-0} != 10#${y[i]:-0})); then
      ((10#${x[i]:-0} > 10#${y[i]:-0}))
      return
    fi
  done
  [ "$ap" = "$bp" ] && return 1
  [ -z "$ap" ] && return 0 # release > prerelease
  [ -z "$bp" ] && return 1
  [ "$(printf '%s\n%s\n' "$ap" "$bp" | sort -V | tail -1)" = "$ap" ]
}

if ! semver_gt "$current" "$released"; then
  echo "check-chart-version: $CHART changed since $latest, but Chart.yaml version $current is not higher than $released." >&2
  echo "Bump version in $CHART/Chart.yaml (hack/bump-chart-version.sh patch|minor|major)." >&2
  exit 1
fi
echo "check-chart-version: chart changed since $latest; version $current > $released"
