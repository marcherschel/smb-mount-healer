# Security policy

smb-mount-healer runs a **privileged** DaemonSet. It mounts the kubelet
directory of every node with bidirectional propagation, so it can unmount and
move directories on the host. The control plane can scale Deployments and
StatefulSets and delete the CSI node driver pods. Please treat security
issues accordingly.

## Reporting a vulnerability

Please do **not** open a public issue. Report it privately through
[GitHub security advisories](https://github.com/marcherschel/smb-mount-healer/security/advisories/new).
Include the version, how to reproduce the issue and its impact. You will get
an answer within a week.

## Supported versions

Only the latest release receives fixes while the project is `0.x`.

## Verifying a release

Every app and chart release is signed with cosign (keyless: the signature's
certificate names the release workflow and the ref it ran on, issued through
GitHub OIDC) and comes with provenance. App releases also ship SBOMs.

### App

```sh
# Image: cosign signature, SLSA provenance, BuildKit SBOM (per platform)
cosign verify ghcr.io/marcherschel/smb-mount-healer:X.Y.Z \
  --certificate-identity-regexp '^https://github\.com/marcherschel/smb-mount-healer/\.github/workflows/release-app\.yaml@refs/(heads/main|tags/v)' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/marcherschel/smb-mount-healer:X.Y.Z --repo marcherschel/smb-mount-healer
docker buildx imagetools inspect ghcr.io/marcherschel/smb-mount-healer:X.Y.Z \
  --format '{{ json (index .SBOM "linux/amd64").SPDX }}'

# Binary from the GitHub release: provenance and SBOM attestation
gh attestation verify smb-mount-healer-linux-amd64 --repo marcherschel/smb-mount-healer
gh attestation verify smb-mount-healer-linux-amd64 --repo marcherschel/smb-mount-healer \
  --predicate-type https://spdx.dev/Document/v2.3
```

The GitHub release also carries the SBOMs as files: per binary
`smb-mount-healer-linux-<arch>.spdx.json` and `.cdx.json` (CycloneDX), per
image platform `smb-mount-healer-image-linux-<arch>.spdx.json`, all listed in
`SHA256SUMS`. Feed them to a scanner, e.g. `grype sbom:smb-mount-healer-linux-amd64.spdx.json`.

### Chart

The chart is an OCI artifact in GHCR, signed like the image (cosign takes the
reference without `oci://`). Artifact Hub shows it as signed.

```sh
# Chart in GHCR: cosign signature
cosign verify ghcr.io/marcherschel/charts/smb-mount-healer:A.B.C \
  --certificate-identity-regexp '^https://github\.com/marcherschel/smb-mount-healer/\.github/workflows/release-chart\.yaml@refs/(heads/main|tags/chart-v)' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# Or pin the digest: verify ghcr.io/...@sha256:<digest> the same way, then
# install exactly that (digest of a version: oras resolve ghcr.io/marcherschel/charts/smb-mount-healer:A.B.C)
helm install smb-mount-healer oci://ghcr.io/marcherschel/charts/smb-mount-healer@sha256:<digest> \
  -n smb-mount-healer-system
```

With Flux, let the source-controller check the signature before it installs:
see the [Flux quick start](production/smb-mount-healer/README.md#quick-start-with-flux)
(`chart.spec.verify`).

### Files on a GitHub release

Starting with the first app and chart releases after chart 0.1.4 and app
v0.1.1, every GitHub release also
carries `SHA256SUMS.sigstore.json` (a cosign bundle signing `SHA256SUMS`, and
through it every file of the release) and a provenance bundle
(`*.intoto.jsonl`):

```sh
# App release vX.Y.Z (for a chart release use the release-chart identity above)
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/marcherschel/smb-mount-healer/\.github/workflows/release-app\.yaml@refs/(heads/main|tags/v)' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing SHA256SUMS

# Chart package from a chart release: provenance
gh attestation verify smb-mount-healer-A.B.C.tgz --repo marcherschel/smb-mount-healer
```

## Hardening notes

- The control plane runs as non-root with a read-only root filesystem; only
  the worker is privileged.
- RBAC is generated from the code's markers and limited to what each
  component needs (`config/rbac/`).
- Host paths are validated: host tasks can only act on paths below the kubelet
  root (the quarantine directory must be below it too).
- No file or directory name from user data is written to any CR, event,
  metric or log line.
