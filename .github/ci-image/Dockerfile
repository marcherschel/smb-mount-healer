# CI toolbox for .github/workflows/ci.yaml and release.yaml (job container).
# Tool versions come from versions.env and tools.env (passed as build args by build.sh and
# .github/workflows/ci-image.yaml).
#
# Downloads run once on the build platform (no emulation); only the final
# apt-get layer runs under the target platform.

FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS tools

ARG TARGETARCH
ARG KUBECTL_VERSION
ARG HELM_VERSION
ARG K3D_VERSION
ARG TILT_VERSION
ARG GOLANGCI_LINT_VERSION
ARG YQ_VERSION
ARG ORAS_VERSION

SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

WORKDIR /dl
RUN mkdir -p /out

# kubectl (sha256 from dl.k8s.io)
RUN : "${KUBECTL_VERSION:?build-arg KUBECTL_VERSION is required}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"; \
    echo "$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256")  kubectl" | sha256sum --check; \
    install -m 0755 kubectl /out/kubectl

# helm (sha256sum file from get.helm.sh)
RUN : "${HELM_VERSION:?build-arg HELM_VERSION is required}"; \
    name="helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLO "https://get.helm.sh/${name}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "https://get.helm.sh/${name}.sha256sum" | sha256sum --check; \
    tar -xzf "${name}" "linux-${TARGETARCH}/helm"; \
    install -m 0755 "linux-${TARGETARCH}/helm" /out/helm

# k3d (checksums.txt, same parsing as hack/tools/install.sh)
RUN : "${K3D_VERSION:?build-arg K3D_VERSION is required}"; \
    name="k3d-linux-${TARGETARCH}"; \
    base="https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLO "${base}/${name}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "${base}/checksums.txt" -o k3d-checksums.txt; \
    expected=$(awk -v f="${name}" '{n=$2; sub(/^\*?(_dist\/)?/, "", n)} n == f {print $1}' k3d-checksums.txt); \
    [ -n "${expected}" ]; echo "${expected}  ${name}" | sha256sum --check; \
    install -m 0755 "${name}" /out/k3d

# tilt (checksums.txt)
RUN : "${TILT_VERSION:?build-arg TILT_VERSION is required}"; \
    case "${TARGETARCH}" in amd64) arch=x86_64 ;; arm64) arch=arm64 ;; *) echo "unsupported ${TARGETARCH}"; exit 1 ;; esac; \
    name="tilt.${TILT_VERSION#v}.linux.${arch}.tar.gz"; \
    base="https://github.com/tilt-dev/tilt/releases/download/${TILT_VERSION}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLO "${base}/${name}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "${base}/checksums.txt" -o tilt-checksums.txt; \
    grep -E "  ${name}\$" tilt-checksums.txt | sha256sum --check; \
    tar -xzf "${name}" tilt; \
    install -m 0755 tilt /out/tilt

# golangci-lint (checksums file, same as hack/tools/install.sh)
RUN : "${GOLANGCI_LINT_VERSION:?build-arg GOLANGCI_LINT_VERSION is required}"; \
    name="golangci-lint-${GOLANGCI_LINT_VERSION#v}-linux-${TARGETARCH}"; \
    base="https://github.com/golangci/golangci-lint/releases/download/${GOLANGCI_LINT_VERSION}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLO "${base}/${name}.tar.gz"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "${base}/golangci-lint-${GOLANGCI_LINT_VERSION#v}-checksums.txt" -o golangci-checksums.txt; \
    grep -E "  ${name}\.tar\.gz\$" golangci-checksums.txt | sha256sum --check; \
    tar -xzf "${name}.tar.gz" "${name}/golangci-lint"; \
    install -m 0755 "${name}/golangci-lint" /out/golangci-lint

# yq (multi-hash "checksums" file; column chosen via checksums_hashes_order)
RUN : "${YQ_VERSION:?build-arg YQ_VERSION is required}"; \
    name="yq_linux_${TARGETARCH}"; \
    base="https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLO "${base}/${name}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "${base}/checksums" -o yq-checksums; \
    idx=$(curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "${base}/checksums_hashes_order" | grep -nx 'SHA-256' | cut -d: -f1); \
    expected=$(awk -v f="${name}" -v c="$((idx + 1))" '$1 == f {print $c}' yq-checksums); \
    [ -n "${expected}" ]; echo "${expected}  ${name}" | sha256sum --check; \
    install -m 0755 "${name}" /out/yq

# oras (checksums file)
RUN : "${ORAS_VERSION:?build-arg ORAS_VERSION is required}"; \
    v="${ORAS_VERSION#v}"; name="oras_${v}_linux_${TARGETARCH}.tar.gz"; \
    base="https://github.com/oras-project/oras/releases/download/${ORAS_VERSION}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSLO "${base}/${name}"; \
    curl --retry 5 --retry-all-errors --retry-delay 2 -fsSL "${base}/oras_${v}_checksums.txt" -o oras-checksums.txt; \
    grep -E "  ${name}\$" oras-checksums.txt | sha256sum --check; \
    tar -xzf "${name}" oras; \
    install -m 0755 oras /out/oras

# docker CLI + buildx plugin (static binaries; version pinned by digest)
FROM docker:29.9.0-cli@sha256:1a4c7cb63513f349bdad01fcc6e0f3f2f67d37b9da86f14dc0d4a0942eecda00 AS docker-cli

FROM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates \
       curl \
       git \
       iproute2 \
       jq \
       make \
       zstd \
    && rm -rf /var/lib/apt/lists/*

COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker-cli /usr/local/libexec/docker/cli-plugins/docker-buildx /usr/local/libexec/docker/cli-plugins/docker-buildx
COPY --from=tools /out/ /usr/local/bin/

# CI uses exactly the Go in this image (go.mod's toolchain line must match).
ENV GOTOOLCHAIN=local

RUN go version \
    && docker --version \
    && docker buildx version \
    && kubectl version --client \
    && helm version \
    && k3d version \
    && tilt version \
    && golangci-lint version \
    && yq --version \
    && oras version
