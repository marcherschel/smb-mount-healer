# smb-mount-healer generated manifests

This directory holds only controller-gen output (`make manifests`); do not edit
it by hand. The healer is deployed with the Helm chart in
[`production/smb-mount-healer`](../production/smb-mount-healer/README.md),
in e2e (`test/e2e/Tiltfile`) and in production.

```
config/
  crd/bases/          CRDs from the API types (api/v1alpha1)
  rbac/role-*.yaml    ClusterRoles from the +kubebuilder:rbac markers in
                      internal/{controlplane,worker}/rbac.go
```

`make manifests` runs controller-gen and then `hack/sync-chart.sh`, which
turns these files into the chart's `templates/crds/*.yaml` and
`templates/rbac/clusterrole-*.yaml` (CRDs wrapped in `crds.install`, with the
chart labels and the `crds.keep` resource policy; ClusterRole `rules:`
verbatim). CI fails when `make generate manifests chart-docs` leaves a diff, so
the chart can never drift from the code. `hack/chart-test.sh test` also checks
that the rendered CRDs and ClusterRole rules equal these files.

The leader-election Role (Leases and Events in the release namespace) is a
static chart template, `templates/rbac/leader-election.yaml`.
