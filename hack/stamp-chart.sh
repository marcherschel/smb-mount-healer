#!/usr/bin/env bash
# Stamps a chart release into production/smb-mount-healer/Chart.yaml (run by
# .github/workflows/release-chart.yaml on the release commit it tags; the
# version on main is a placeholder and never bumped by hand).
#
#   hack/stamp-chart.sh <version> <appVersion> [previous chart tag]
#
# Sets `version`, `appVersion` (the app release the chart deploys by default),
# the artifacthub.io/images tag and artifacthub.io/prerelease. With a previous
# chart tag, artifacthub.io/changes keeps only the entries that release did not
# already list (so entries never have to be removed from main after a release)
# and gets a "Default image ..." entry when appVersion moved.
set -euo pipefail

cd "$(dirname "$0")/.."

CHART=production/smb-mount-healer/Chart.yaml
IMAGE=ghcr.io/marcherschel/smb-mount-healer
version=${1:?usage: $0 <version> <appVersion> [previous chart tag]}
app_version=${2:?usage: $0 <version> <appVersion> [previous chart tag]}
previous=${3:-}

semver='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
[[ "$version" =~ $semver ]] || { echo "stamp-chart: version '$version' is not SemVer" >&2; exit 1; }
[[ "$app_version" =~ $semver ]] || { echo "stamp-chart: appVersion '$app_version' is not SemVer" >&2; exit 1; }

prerelease=false
[[ "$version" == *-* ]] && prerelease=true

changes=$(yq '.annotations."artifacthub.io/changes" // "[]"' "$CHART" | yq -o json -I0 '. // []')
if [ -n "$previous" ]; then
  prev_chart=$(git show "$previous:$CHART")
  prev_changes=$(yq '.annotations."artifacthub.io/changes" // "[]"' <<<"$prev_chart" | yq -o json -I0 '. // []')
  prev_app=$(yq '.appVersion' <<<"$prev_chart")
  changes=$(PREV="$prev_changes" yq -p json -o json -I0 \
    '[.[] | select(. as $e | env(PREV) | any_c(.description == $e.description) | not)]' <<<"$changes")
  if [ "$prev_app" != "$app_version" ]; then
    changes=$(DESC="Default image ${IMAGE}:${app_version}." yq -p json -o json -I0 \
      'select(any_c(.description == strenv(DESC)) | not) += [{"kind": "changed", "description": strenv(DESC)}]' <<<"$changes")
  fi
fi
changes_yaml=$(yq -p json -o yaml '.' <<<"$changes")
[ "$changes_yaml" = "[]" ] && changes_yaml=""

VERSION="$version" APP="$app_version" PRE="$prerelease" CHANGES="$changes_yaml" IMG="${IMAGE}:${app_version}" yq -i '
  .version = strenv(VERSION) |
  .appVersion = strenv(APP) | .appVersion style="double" |
  .annotations."artifacthub.io/prerelease" = strenv(PRE) | .annotations."artifacthub.io/prerelease" style="double" |
  .annotations."artifacthub.io/images" = (
    .annotations."artifacthub.io/images" | from_yaml | .[0].image = strenv(IMG) | to_yaml) |
  .annotations."artifacthub.io/images" style="literal" |
  .annotations."artifacthub.io/changes" = (strenv(CHANGES) + "\n") |
  .annotations."artifacthub.io/changes" style="literal" |
  del(.annotations."artifacthub.io/changes" | select(. == "\n"))
' "$CHART"

echo "stamp-chart: version $version, appVersion $app_version (prerelease $prerelease)${previous:+, changes since $previous}"
