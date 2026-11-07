# One multi-arch image for both modes (--controlplane | --worker).
#   make image                                              # native arch, loaded into docker
#   make image PLATFORMS=linux/amd64,linux/arm64 IMAGE_OUTPUT=--push
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-trimpath

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/smb-mount-healer ./cmd/smb-mount-healer

# Runtime: distroless static (no shell). Default user is root because the
# worker runs privileged; the control-plane manifest sets runAsNonRoot,
# runAsUser 65532, readOnlyRootFilesystem and drops all capabilities.
FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2

ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="smb-mount-healer" \
      org.opencontainers.image.description="Detects and heals broken SMB CSI mounts (controlplane + worker)" \
      org.opencontainers.image.source="https://github.com/marcherschel/smb-mount-healer" \
      org.opencontainers.image.licenses="GPL-2.0-only" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/smb-mount-healer /smb-mount-healer
ENTRYPOINT ["/smb-mount-healer"]
