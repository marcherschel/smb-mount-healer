#!/usr/bin/env bash
# Builds the CI image locally from versions.env (CI builds it with
# .github/workflows/ci-image.yaml).
#
#   .github/ci-image/build.sh                 # native platform, loaded into docker
#   OUTPUT=--push IMAGE=ghcr.io/<owner>/smb-mount-healer-ci:<tag> PLATFORMS=linux/amd64,linux/arm64 .github/ci-image/build.sh
#   OUTPUT= PLATFORMS=linux/amd64,linux/arm64 .github/ci-image/build.sh   # build only (multi-platform cannot --load)
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$DIR/../.." && pwd)
# shellcheck disable=SC1091
source "$ROOT/versions.env"

IMAGE=${IMAGE:-smb-mount-healer-ci:local}
# OUTPUT unset: --load; set but empty: build only.
output=()
[ -n "${OUTPUT---load}" ] && output=("${OUTPUT---load}")

docker buildx build \
  --platform "${PLATFORMS:-linux/$(docker version --format '{{.Server.Arch}}')}" \
  --build-arg KUBECTL_VERSION="v${KUBERNETES_VERSION#v}" \
  --build-arg HELM_VERSION="$HELM_VERSION" \
  --build-arg K3D_VERSION="$K3D_VERSION" \
  --build-arg TILT_VERSION="$TILT_VERSION" \
  --build-arg GOLANGCI_LINT_VERSION="$GOLANGCI_LINT_VERSION" \
  --build-arg YQ_VERSION="$YQ_VERSION" \
  --build-arg ORAS_VERSION="$ORAS_VERSION" \
  -t "$IMAGE" "${output[@]}" "$DIR"
