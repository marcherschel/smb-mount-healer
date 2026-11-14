#!/usr/bin/env bash
# Prints the next release version (without tag prefix), computed from the
# existing git tags, so no release ever needs a hand-picked version.
#
#   hack/next-version.sh app|chart patch|minor|major|rc [prerelease]
#
#   app    tags vX.Y.Z[-rc.N]        (release-app.yaml)
#   chart  tags chart-vX.Y.Z[-rc.N]  (release-chart.yaml)
#
# patch|minor|major  bump the latest stable release (an open release candidate
#                    of the resulting version is thereby promoted: 0.2.0-rc.2
#                    and a minor bump from 0.1.4 give 0.2.0)
# ... prerelease     with a third argument "true": the next release candidate
#                    of that version instead (0.2.0-rc.1, or rc.N+1 if
#                    candidates of 0.2.0 exist)
# rc                 the next candidate of the open release candidate line
#                    (0.2.0-rc.2 -> 0.2.0-rc.3); fails if none is open
#
# Without any release yet, the bump starts from 0.0.0. Needs the tags
# (fetch-depth: 0 in CI).
set -euo pipefail

cd "$(dirname "$0")/.."

usage() {
  echo "usage: $0 app|chart patch|minor|major|rc [prerelease]" >&2
  exit 2
}

case "${1:-}" in
  app) prefix=v ;;
  chart) prefix=chart-v ;;
  *) usage ;;
esac
bump=${2:-}
prerelease=${3:-false}
case "$prerelease" in true | false) ;; *) usage ;; esac

# Released versions, newest first. versionsort.suffix=-: a release candidate
# (1.0.0-rc.1) sorts before its release (1.0.0).
versions=$(git -c versionsort.suffix=- tag -l "${prefix}[0-9]*" --sort=-v:refname |
  sed -n "s/^${prefix}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\(-rc\.[0-9][0-9]*\)\{0,1\}\)$/\1/p")
latest=$(head -1 <<<"$versions")
stable=$(grep -v "[-]" <<<"$versions" | head -1 || true)

# next_rc X.Y.Z: X.Y.Z-rc.(highest existing N + 1), or X.Y.Z-rc.1.
next_rc() {
  local n
  n=$(grep -F -- "$1-rc." <<<"$versions" | sed 's/.*-rc\.//' | sort -n | tail -1 || true)
  echo "$1-rc.$((${n:-0} + 1))"
}

case "$bump" in
  rc)
    if [[ "$latest" != *-rc.* ]]; then
      echo "next-version: no open release candidate (latest ${prefix}${latest:-<none>}); start one with patch|minor|major and prerelease" >&2
      exit 1
    fi
    next_rc "${latest%-rc.*}"
    exit 0
    ;;
  patch | minor | major) ;;
  *) usage ;;
esac

IFS=. read -r major minor patch <<<"${stable:-0.0.0}"
case "$bump" in
  patch) patch=$((patch + 1)) ;;
  minor) minor=$((minor + 1)) patch=0 ;;
  major) major=$((major + 1)) minor=0 patch=0 ;;
esac
next="$major.$minor.$patch"

if [ "$prerelease" = true ]; then
  next_rc "$next"
else
  echo "$next"
fi
