#!/usr/bin/env bash
# Builds the app release assets into dist/app (run by `make dist-app`, used by
# .github/workflows/release-app.yaml):
#
#   smb-mount-healer-linux-<arch>   static binaries (CGO off), one per PLATFORMS entry
#   smb-mount-healer-crds.yaml      the four CRDs of this API version (config/crd/bases)
#   SHA256SUMS
#
# Environment:
#   VERSION    app version without "v" (required, e.g. 0.1.0 or 0.1.0-rc.1)
#   REVISION   git revision for --version (default: git rev-parse --short HEAD)
#   PLATFORMS  space-separated os/arch list (default: "linux/amd64 linux/arm64")
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION=${VERSION:?VERSION is required (e.g. VERSION=0.1.0)}
VERSION=${VERSION#v}
if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "dist-app: VERSION=$VERSION is not SemVer X.Y.Z[-pre]" >&2
  exit 1
fi
REVISION=${REVISION:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
PLATFORMS=${PLATFORMS:-linux/amd64 linux/arm64}
OUT=dist/app

rm -rf "$OUT"
mkdir -p "$OUT"

for platform in $PLATFORMS; do
  os=${platform%/*} arch=${platform#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
    -o "$OUT/smb-mount-healer-$os-$arch" ./cmd/smb-mount-healer
done

for f in config/crd/bases/*.yaml; do
  echo '---'
  cat "$f"
done >"$OUT/smb-mount-healer-crds.yaml"

(cd "$OUT" && sha256sum -- * >SHA256SUMS)
echo "dist-app: $(ls "$OUT" | xargs)"
