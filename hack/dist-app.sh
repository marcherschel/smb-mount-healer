#!/usr/bin/env bash
# Builds the app release assets into dist/app (run by `make dist-app`, used by
# .github/workflows/release-app.yaml):
#
#   smb-mount-healer-linux-<arch>   static binaries (CGO off), one per PLATFORMS entry
#   smb-mount-healer-linux-<arch>.spdx.json  SBOM of each binary (syft: Go
#   smb-mount-healer-linux-<arch>.cdx.json   toolchain and modules), SPDX and CycloneDX
#   smb-mount-healer-image-linux-<arch>.spdx.json  SBOM of the image per platform,
#                                   as BuildKit attached it (only with SBOM_IMAGE)
#   smb-mount-healer-crds.yaml      the four CRDs of this API version (config/crd/bases)
#   SHA256SUMS
#
# Environment:
#   VERSION     app version without "v" (required, e.g. 0.1.0 or 0.1.0-rc.1)
#   REVISION    git revision for --version (default: git rev-parse --short HEAD)
#   PLATFORMS   space-separated os/arch list (default: "linux/amd64 linux/arm64")
#   SYFT        syft binary (default: syft; `make dist-app` passes the pinned one)
#   SBOM_IMAGE  optional pushed image (repo@sha256:...) built with --sbom=true;
#               its per-platform SBOMs are exported (docker buildx imagetools)
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
SYFT=${SYFT:-syft}
SBOM_IMAGE=${SBOM_IMAGE:-}
OUT=dist/app

rm -rf "$OUT"
mkdir -p "$OUT"

for platform in $PLATFORMS; do
  os=${platform%/*} arch=${platform#*/}
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -X main.revision=$REVISION" \
    -o "$OUT/smb-mount-healer-$os-$arch" ./cmd/smb-mount-healer
done

# Binary SBOMs from the Go build info embedded in each binary: exactly the
# toolchain and module versions linked in, no test-only dependencies.
for platform in $PLATFORMS; do
  bin="$OUT/smb-mount-healer-${platform%/*}-${platform#*/}"
  SYFT_CHECK_FOR_APP_UPDATE=false "$SYFT" scan "file:$bin" -q \
    --source-name smb-mount-healer --source-version "$VERSION" \
    -o "spdx-json=$bin.spdx.json" -o "cyclonedx-json=$bin.cdx.json"
  grep -q '"name": *"github.com/marcherschel/smb-mount-healer"' "$bin.spdx.json" || {
    echo "dist-app: $bin.spdx.json lists no smb-mount-healer module (no Go build info?)" >&2
    exit 1
  }
done

# Image SBOMs: the ones BuildKit attached to the pushed image (base image
# packages included), one per platform.
if [ -n "$SBOM_IMAGE" ]; then
  for platform in $PLATFORMS; do
    f="$OUT/smb-mount-healer-image-${platform%/*}-${platform#*/}.spdx.json"
    docker buildx imagetools inspect "$SBOM_IMAGE" \
      --format "{{ json (index .SBOM \"$platform\").SPDX }}" >"$f"
    grep -q '"spdxVersion"' "$f" || {
      echo "dist-app: no $platform SBOM attached to $SBOM_IMAGE (built without --sbom=true?)" >&2
      exit 1
    }
  done
fi

for f in config/crd/bases/*.yaml; do
  echo '---'
  cat "$f"
done >"$OUT/smb-mount-healer-crds.yaml"

(cd "$OUT" && sha256sum -- * >SHA256SUMS)
echo "dist-app: $(ls "$OUT" | xargs)"
