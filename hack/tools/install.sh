#!/usr/bin/env bash
# Installs the pinned developer tools from versions.env and tools.env, sha256-verified.
#
#   hack/tools/install.sh [tool...]   tools: kubectl helm k3d tilt golangci-lint yq oras
#                                     (default: all of them)
#
# Environment:
#   INSTALL_DIR  target directory (default: ~/.local/bin)
#   BIN_NAME     file name for a single tool (default: the tool name); the
#                Makefile uses it for its versioned bin/<tool>-<version> copies
#   <TOOL>_VERSION (e.g. HELM_VERSION) overrides the pin
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# Keep versions given in the environment (the Makefile passes the exact one).
while IFS='=' read -r key value; do
  [[ -z "$key" || "$key" == \#* ]] && continue
  [[ -z "${!key:-}" ]] && export "$key=$value"
done < <(cat "$ROOT/versions.env" "$ROOT/tools.env")

INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  arm64 | aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# verify <file> <expected-sha256>
verify() {
  local actual
  actual=$(sha256sum "$1" | awk '{print $1}')
  if [[ -z "$2" || "$2" != "$actual" ]]; then
    echo "checksum mismatch for $(basename "$1"): expected '$2', got '$actual'" >&2
    exit 1
  fi
}

# place <tool> <file>
place() {
  mkdir -p "$INSTALL_DIR"
  install -m 0755 "$2" "$INSTALL_DIR/${BIN_NAME:-$1}"
  echo "installed $1 -> $INSTALL_DIR/${BIN_NAME:-$1}"
}

install_kubectl() {
  local base="https://dl.k8s.io/release/v${KUBERNETES_VERSION#v}/bin/linux/${ARCH}"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/kubectl" "$base/kubectl"
  verify "$TMP/kubectl" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/kubectl.sha256")"
  place kubectl "$TMP/kubectl"
}

install_helm() {
  local name="helm-${HELM_VERSION}-linux-${ARCH}.tar.gz"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/$name" "https://get.helm.sh/$name"
  verify "$TMP/$name" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "https://get.helm.sh/$name.sha256sum" | awk '{print $1}')"
  tar -xzf "$TMP/$name" -C "$TMP" "linux-${ARCH}/helm"
  place helm "$TMP/linux-${ARCH}/helm"
}

install_k3d() {
  local name="k3d-linux-${ARCH}" base="https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/$name" "$base/$name"
  verify "$TMP/$name" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/checksums.txt" |
    awk -v f="$name" '{n=$2; sub(/^\*?(_dist\/)?/, "", n)} n == f {print $1}')"
  place k3d "$TMP/$name"
}

install_tilt() {
  local arch=$ARCH
  [[ $arch == amd64 ]] && arch=x86_64
  local name="tilt.${TILT_VERSION#v}.linux.${arch}.tar.gz" base="https://github.com/tilt-dev/tilt/releases/download/${TILT_VERSION}"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/$name" "$base/$name"
  verify "$TMP/$name" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/checksums.txt" | awk -v f="$name" '$2 == f {print $1}')"
  tar -xzf "$TMP/$name" -C "$TMP" tilt
  place tilt "$TMP/tilt"
}

install_golangci-lint() {
  local v=${GOLANGCI_LINT_VERSION#v}
  local name="golangci-lint-${v}-linux-${ARCH}" base="https://github.com/golangci/golangci-lint/releases/download/${GOLANGCI_LINT_VERSION}"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/$name.tar.gz" "$base/$name.tar.gz"
  verify "$TMP/$name.tar.gz" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/golangci-lint-${v}-checksums.txt" |
    awk -v f="$name.tar.gz" '$2 == f {print $1}')"
  tar -xzf "$TMP/$name.tar.gz" -C "$TMP" "$name/golangci-lint"
  place golangci-lint "$TMP/$name/golangci-lint"
}

install_yq() {
  local name="yq_linux_${ARCH}" base="https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/$name" "$base/$name"
  local idx
  idx=$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/checksums_hashes_order" | grep -nx 'SHA-256' | cut -d: -f1)
  verify "$TMP/$name" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/checksums" | awk -v f="$name" -v c="$((idx + 1))" '$1 == f {print $c}')"
  place yq "$TMP/$name"
}

install_oras() {
  local v=${ORAS_VERSION#v}
  local name="oras_${v}_linux_${ARCH}.tar.gz" base="https://github.com/oras-project/oras/releases/download/${ORAS_VERSION}"
  curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo "$TMP/$name" "$base/$name"
  verify "$TMP/$name" "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "$base/oras_${v}_checksums.txt" | awk -v f="$name" '$2 == f {print $1}')"
  tar -xzf "$TMP/$name" -C "$TMP" oras
  place oras "$TMP/oras"
}

tools=("$@")
[[ ${#tools[@]} -eq 0 ]] && tools=(kubectl helm k3d tilt golangci-lint yq oras)
if [[ ${#tools[@]} -gt 1 && -n "${BIN_NAME:-}" ]]; then
  echo "BIN_NAME only works with a single tool" >&2
  exit 2
fi
for t in "${tools[@]}"; do
  case "$t" in
    kubectl | helm | k3d | tilt | golangci-lint | yq | oras) "install_$t" ;;
    *) echo "unknown tool: $t" >&2; exit 2 ;;
  esac
done
