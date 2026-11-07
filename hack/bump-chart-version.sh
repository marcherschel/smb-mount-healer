#!/usr/bin/env bash
# Bumps the chart version in production/smb-mount-healer/Chart.yaml.
#
#   hack/bump-chart-version.sh patch|minor|major [change description]
#
# With a description, it also appends a `changed` entry to the
# artifacthub.io/changes annotation. Renovate runs it as a postUpgradeTask after
# updating appVersion to a new app release (see renovate.json), so it uses only
# sed/awk (the Renovate image has no yq).
set -euo pipefail

cd "$(dirname "$0")/.."

FILE=production/smb-mount-healer/Chart.yaml
part=${1:?usage: $0 patch|minor|major [change description]}
desc=${2:-}

current=$(sed -n 's/^version:[[:space:]]*"\{0,1\}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' "$FILE")
[ -n "$current" ] || { echo "bump-chart-version: no X.Y.Z version in $FILE" >&2; exit 1; }
IFS=. read -r major minor patch <<<"$current"
case "$part" in
  patch) patch=$((patch + 1)) ;;
  minor) minor=$((minor + 1)) patch=0 ;;
  major) major=$((major + 1)) minor=0 patch=0 ;;
  *) echo "bump-chart-version: unknown part '$part'" >&2; exit 2 ;;
esac
next="$major.$minor.$patch"
sed -i "s/^version:.*/version: $next/" "$FILE"

if [ -n "$desc" ]; then
  # Append "- kind: changed / description: ..." at the end of the
  # artifacthub.io/changes block scalar (entries are indented by 4 spaces).
  awk -v desc="$desc" '
    function flush() { if (inblock) { print "    - kind: changed"; print "      description: " desc; inblock = 0 } }
    /^  artifacthub\.io\/changes: \|/ { print; inblock = 1; next }
    inblock && !/^    / { flush() }
    { print }
    END { flush() }
  ' "$FILE" >"$FILE.tmp" && mv "$FILE.tmp" "$FILE"
fi
echo "bump-chart-version: $current -> $next"
