#!/usr/bin/env bash
# Lints and tests the Helm chart in production/smb-mount-healer without Helm
# plugins (works with Helm 4).
#
#   hack/chart-test.sh lint   helm lint --strict with every ci/*-values.yaml
#   hack/chart-test.sh test   helm template every ci/*-values.yaml, validate
#                             with kubeconform, and assert on the rendered output
#
# Environment:
#   HELM         helm binary (default: helm)
#   KUBECONFORM  kubeconform command (default: the pinned image via docker)
#   KUBE_VERSION Kubernetes version for rendering and schemas
#                (default: KUBERNETES_VERSION from versions.env)
set -euo pipefail

cd "$(dirname "$0")/.."

CHART=production/smb-mount-healer
HELM=${HELM:-helm}
if [ -z "${KUBE_VERSION:-}" ]; then
  KUBE_VERSION=$(sed -n 's/^KUBERNETES_VERSION=//p' versions.env)
fi
KUBE_VERSION=${KUBE_VERSION#v}
# https://github.com/yannh/kubeconform/pkgs/container/kubeconform
# renovate: datasource=docker depName=ghcr.io/yannh/kubeconform
KUBECONFORM_IMAGE=ghcr.io/yannh/kubeconform:v0.7.0@sha256:85dbef6b4b312b99133decc9c6fc9495e9fc5f92293d4ff3b7e1b30f5611823c
KUBECONFORM=${KUBECONFORM:-docker run --rm -i $KUBECONFORM_IMAGE}
PODMONITOR_API=monitoring.coreos.com/v1/PodMonitor
PROMETHEUSRULE_API=monitoring.coreos.com/v1/PrometheusRule
# promtool validates the alerting rules (monitoring/alerts, rendered PrometheusRule).
# https://hub.docker.com/r/prom/prometheus/tags
# renovate: datasource=docker depName=prom/prometheus
PROMTOOL_IMAGE=prom/prometheus:v3.16.0@sha256:e04987641b57704f408e31f6373ba4138ecf27cb4a165d24aabb6c38fddd0f06
PROMTOOL=${PROMTOOL:-docker run --rm -i --entrypoint /bin/promtool $PROMTOOL_IMAGE}
RELEASE=smb-mount-healer
NS=test-ns

command -v "$HELM" >/dev/null || { echo "chart-test: $HELM not found" >&2; exit 1; }
command -v yq >/dev/null || { echo "chart-test: yq is required" >&2; exit 1; }
command -v jq >/dev/null || { echo "chart-test: jq is required" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { echo "  ok   $*"; }
fail() { echo "  FAIL $*" >&2; failures=$((failures + 1)); }
# check <description> <expected> <actual>
check() {
  if [ "$2" == "$3" ]; then pass "$1"; else fail "$1: expected '$2', got '$3'"; fi
}

# Per values file: extra helm flags. helm lint cannot set API versions, so the
# PodMonitor file is linted with the PodMonitors switched off; `test` renders
# it with the PodMonitor API available.
lint_flags() {
  case "$(basename "$1")" in
    monitoring*-values.yaml) echo "--set metrics.podMonitor.enabled=false --set monitoring.prometheusRule.enabled=false" ;;
  esac
}
template_flags() {
  case "$(basename "$1")" in
    monitoring*-values.yaml) echo "--api-versions $PODMONITOR_API --api-versions $PROMETHEUSRULE_API" ;;
  esac
}

render() { # render <out> <helm template args...>
  local out=$1
  shift
  "$HELM" template "$RELEASE" "$CHART" -n "$NS" --kube-version "$KUBE_VERSION" "$@" >"$out"
}

# --- lint ---------------------------------------------------------------------
cmd_lint() {
  local f
  for f in "$CHART"/ci/*-values.yaml; do
    echo "helm lint --strict -f $f"
    # shellcheck disable=SC2046
    "$HELM" lint --strict --quiet "$CHART" --kube-version "$KUBE_VERSION" -f "$f" $(lint_flags "$f") \
      || fail "helm lint $f"
  done
}

# --- test ---------------------------------------------------------------------
# The published Kubernetes JSON schemas have no CustomResourceDefinition
# schema, so CRDs are skipped here; the "CRDs match config/crd/bases"
# assertion below covers them (and envtest installs config/crd/bases).
kubeconform() { # kubeconform <file>
  # shellcheck disable=SC2086
  $KUBECONFORM -strict -summary -kubernetes-version "$KUBE_VERSION" -skip CustomResourceDefinition \
    -schema-location default \
    -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
    - <"$1"
}

# sel <file> <kind> <yq expr on the selected object>
sel() { yq -N "select(.kind == \"$2\") | $3" "$1"; }

cmd_test() {
  local f name out
  for f in "$CHART"/ci/*-values.yaml; do
    name=$(basename "$f" -values.yaml)
    out="$TMP/$name.yaml"
    echo "helm template -f $f"
    # shellcheck disable=SC2046
    if ! render "$out" -f "$f" $(template_flags "$f"); then
      fail "helm template $f"
      continue
    fi
    if kubeconform "$out"; then pass "kubeconform $name"; else fail "kubeconform $name"; fi
  done

  local d="$TMP/default.yaml" cp='select(.kind == "Deployment") | .spec.template.spec.containers[0]'
  echo "assertions: default"
  check "no --dry-run arg" "0" "$(yq -N "$cp | .args[] | select(. == \"--dry-run\")" "$d" | wc -l | tr -d ' ')"
  check "dry-run annotation (string)" '"false"' "$(yq -N -o json 'select(.kind == "Deployment") | .metadata.annotations."smbhealer.herschel.io/dry-run"' "$d")"
  check "4 CRDs" "4" "$(sel "$d" CustomResourceDefinition '.metadata.name' | wc -l | tr -d ' ')"
  check "every CRD has the keep policy" "keep keep keep keep" \
    "$(sel "$d" CustomResourceDefinition '.metadata.annotations."helm.sh/resource-policy"' | xargs)"
  check "no PodMonitor by default" "0" "$(sel "$d" PodMonitor '.metadata.name' | wc -l | tr -d ' ')"
  check "no dashboard ConfigMap by default" "0" "$(sel "$d" ConfigMap '.metadata.name' | wc -l | tr -d ' ')"
  check "no PrometheusRule by default" "0" "$(sel "$d" PrometheusRule '.metadata.name' | wc -l | tr -d ' ')"
  check "image defaults to appVersion" "ghcr.io/marcherschel/smb-mount-healer:$(yq '.appVersion' "$CHART/Chart.yaml")" \
    "$(yq -N "$cp | .image" "$d")"
  check "artifacthub.io/images tag == appVersion" "ghcr.io/marcherschel/smb-mount-healer:$(yq '.appVersion' "$CHART/Chart.yaml")" \
    "$(yq '.annotations."artifacthub.io/images"' "$CHART/Chart.yaml" | yq '.[0].image')"
  local w='select(.kind == "DaemonSet") | .spec.template.spec'
  local root
  root=$(yq '.kubeletRoot' "$CHART/values.yaml")
  check "worker hostPath == kubeletRoot" "$root" "$(yq -N "$w | .volumes[] | select(.name == \"kubelet-dir\") | .hostPath.path" "$d")"
  check "worker mountPath == kubeletRoot" "$root" "$(yq -N "$w | .containers[0].volumeMounts[] | select(.name == \"kubelet-dir\") | .mountPath" "$d")"
  check "worker mount is Bidirectional" "Bidirectional" "$(yq -N "$w | .containers[0].volumeMounts[] | select(.name == \"kubelet-dir\") | .mountPropagation" "$d")"
  check "worker is privileged" "true" "$(yq -N "$w | .containers[0].securityContext.privileged" "$d")"
  check "worker KUBELET_ROOT == kubeletRoot" "$root" "$(yq -N "$w | .containers[0].env[] | select(.name == \"KUBELET_ROOT\") | .value" "$d")"
  check "pods selected by component label" "controlplane worker" \
    "$(yq -N 'select(.kind == "Deployment" or .kind == "DaemonSet") | .spec.selector.matchLabels."app.kubernetes.io/component"' "$d" | sort | xargs)"
  # Namespaces: rendered with -n $NS, every namespaced object must be in it and
  # every ServiceAccount subject must point at it.
  local cluster='(.kind == "CustomResourceDefinition" or .kind == "ClusterRole" or .kind == "ClusterRoleBinding")'
  check "namespaced objects all in $NS" "" \
    "$(yq -N "select($cluster | not) | select(.metadata.namespace != \"$NS\") | .kind + \"/\" + .metadata.name" "$d")"
  check "cluster-scoped objects have no namespace" "" \
    "$(yq -N "select($cluster) | select(.metadata.namespace != null) | .kind + \"/\" + .metadata.name" "$d")"
  check "binding subjects in $NS" "" \
    "$(yq -N "select(.subjects != null) | .subjects[] | select(.namespace != \"$NS\") | .name" "$d")"
  check "no hardcoded namespace" "0" "$(grep -cE 'smb-mount-healer-system|namespace: default\b' "$d" || true)"
  render "$TMP/root.yaml" --set kubeletRoot=/var/lib/k0s/kubelet
  check "custom kubeletRoot: hostPath == mountPath" "/var/lib/k0s/kubelet /var/lib/k0s/kubelet" \
    "$(yq -N "$w | ((.volumes[] | select(.name == \"kubelet-dir\") | .hostPath.path), (.containers[0].volumeMounts[] | select(.name == \"kubelet-dir\") | .mountPath))" "$TMP/root.yaml" | xargs)"
  # The chart's CRDs and ClusterRoles are generated from config/ (make manifests).
  local src kind plural
  for src in config/crd/bases/smbhealer.herschel.io_*.yaml; do
    plural=$(basename "$src" .yaml)
    plural=${plural#smbhealer.herschel.io_}
    if diff -q <(yq -o json '.spec' "$src") \
      <(yq -o json "select(.kind == \"CustomResourceDefinition\" and .metadata.name == \"$plural.smbhealer.herschel.io\") | .spec" "$d") >/dev/null; then
      pass "CRD $plural matches $src"
    else
      fail "CRD $plural differs from $src (run make manifests)"
    fi
  done
  for kind in controlplane worker; do
    if diff -q <(yq -o json '.rules' "config/rbac/role-$kind.yaml") \
      <(yq -o json "select(.kind == \"ClusterRole\" and .metadata.name == \"$RELEASE-$kind\") | .rules" "$d") >/dev/null; then
      pass "ClusterRole $kind matches config/rbac/role-$kind.yaml"
    else
      fail "ClusterRole $kind differs from config/rbac/role-$kind.yaml (run make manifests)"
    fi
  done

  # helm package ships the chart's own LICENSE; it must stay the project license.
  if cmp -s LICENSE "$CHART/LICENSE"; then pass "chart LICENSE == LICENSE"; else fail "$CHART/LICENSE differs from LICENSE"; fi

  echo "assertions: dryrun"
  check "--dry-run arg" "1" "$(yq -N "$cp | .args[] | select(. == \"--dry-run\")" "$TMP/dryrun.yaml" | wc -l | tr -d ' ')"
  check "dry-run annotation (string)" '"true"' "$(yq -N -o json 'select(.kind == "Deployment") | .metadata.annotations."smbhealer.herschel.io/dry-run"' "$TMP/dryrun.yaml")"

  echo "assertions: no-crds"
  check "no CRDs" "0" "$(sel "$TMP/no-crds.yaml" CustomResourceDefinition '.metadata.name' | wc -l | tr -d ' ')"

  echo "assertions: monitoring"
  local m="$TMP/monitoring.yaml"
  check "2 PodMonitors" "2" "$(sel "$m" PodMonitor '.metadata.name' | wc -l | tr -d ' ')"
  check "PodMonitor labels" "kube-prometheus-stack kube-prometheus-stack" "$(sel "$m" PodMonitor '.metadata.labels.release' | xargs)"
  check "PodMonitor port/path/interval" "http-metrics /metrics 30s http-metrics /metrics 30s" \
    "$(sel "$m" PodMonitor '.spec.podMetricsEndpoints[0] | .port + " " + .path + " " + .interval' | xargs)"
  check "PodMonitor selects its component" "controlplane worker" \
    "$(sel "$m" PodMonitor '.spec.selector.matchLabels."app.kubernetes.io/component"' | sort | xargs)"

  # Dashboard: the ConfigMap carries monitoring/dashboards/*.json byte for byte.
  local dash_cm='select(.kind == "ConfigMap" and .metadata.name == "smb-mount-healer-dashboard")'
  check "dashboard ConfigMap label" "1" "$(yq -N "$dash_cm | .metadata.labels.grafana_dashboard" "$m")"
  check "dashboard ConfigMap in release namespace" "$NS" "$(yq -N "$dash_cm | .metadata.namespace" "$m")"
  yq -N "$dash_cm | .data.\"smb-mount-healer.json\"" "$m" >"$TMP/dashboard.json"
  if diff -q <(jq -S . monitoring/dashboards/smb-mount-healer.json) <(jq -S . "$TMP/dashboard.json") >/dev/null 2>&1; then
    pass "dashboard JSON == monitoring/dashboards/smb-mount-healer.json"
  else
    fail "dashboard JSON differs from monitoring/dashboards/smb-mount-healer.json (run make manifests)"
  fi
  check "dashboard uid" "smbmounthealer" "$(jq -r .uid "$TMP/dashboard.json" 2>/dev/null)"
  # Alerts: same rules as monitoring/alerts, runbook URLs from runbookBaseUrl.
  local pr='.'
  sel "$m" PrometheusRule '.' >"$TMP/pr.yaml"
  sel "$TMP/monitoring-custom.yaml" PrometheusRule '.' >"$TMP/pr-custom.yaml"
  check "PrometheusRule has the 4 alerts" "$(yq '[.groups[].rules[].alert] | sort | join(" ")' monitoring/alerts/smb-mount-healer.rules.yaml)" \
    "$(yq -N "$pr | [.spec.groups[].rules[].alert] | sort | join(\" \")" "$TMP/pr.yaml")"
  check "PrometheusRule exprs == rule file" "$(yq -o json '[.groups[].rules[] | .expr]' monitoring/alerts/smb-mount-healer.rules.yaml)" \
    "$(yq -N -o json "$pr | [.spec.groups[].rules[] | .expr]" "$TMP/pr.yaml")"
  check "runbook_url per alert" "https://github.com/marcherschel/smb-mount-healer/blob/main/docs/runbooks/SmbMountHealerRemediationLoop.md" \
    "$(yq -N "$pr | .spec.groups[].rules[] | select(.alert == \"SmbMountHealerRemediationLoop\") | .annotations.runbook_url" "$TMP/pr.yaml")"
  local rb
  for rb in $(yq '.groups[].rules[].alert' monitoring/alerts/smb-mount-healer.rules.yaml); do
    if [ -f "docs/runbooks/$rb.md" ]; then pass "runbook docs/runbooks/$rb.md exists"; else fail "runbook docs/runbooks/$rb.md is missing"; fi
  done
  check "Prometheus templates stay literal" '{{ $labels.target_node }}' \
    "$(yq -N "$pr | .spec.groups[].rules[] | select(.alert == \"SmbMountHealerRemediationLoop\") | .annotations.description" "$TMP/pr.yaml" | grep -oF '{{ $labels.target_node }}' | head -1)"
  yq -N ".spec" "$TMP/pr.yaml" >"$TMP/rules.yaml"
  # shellcheck disable=SC2086
  if $PROMTOOL check rules <"$TMP/rules.yaml" >"$TMP/promtool.out" 2>&1; then
    pass "promtool check rules (rendered PrometheusRule)"
  else
    fail "promtool check rules: $(cat "$TMP/promtool.out")"
  fi
  # shellcheck disable=SC2086
  if $PROMTOOL check rules <monitoring/alerts/smb-mount-healer.rules.yaml >"$TMP/promtool.out" 2>&1; then
    pass "promtool check rules (monitoring/alerts/smb-mount-healer.rules.yaml)"
  else
    fail "promtool check rules monitoring/alerts: $(cat "$TMP/promtool.out")"
  fi

  echo "assertions: monitoring-custom"
  local mc="$TMP/monitoring-custom.yaml"
  check "dashboard ConfigMap namespace/label/annotation" "grafana true Storage" \
    "$(yq -N 'select(.kind == "ConfigMap") | .metadata.namespace + " " + .metadata.labels.grafana_dashboard + " " + .metadata.annotations.grafana_folder' "$mc")"
  check "PrometheusRule namespace/label" "monitoring kube-prometheus-stack" \
    "$(yq -N "$pr | .metadata.namespace + \" \" + .metadata.labels.release" "$TMP/pr-custom.yaml")"
  check "disabled alert left out" "3 0" \
    "$(yq -N "$pr | ([.spec.groups[].rules[]] | length) + \" \" + ([.spec.groups[].rules[] | select(.alert == \"SmbMountHealerShadowDataPending\")] | length)" "$TMP/pr-custom.yaml")"
  check "additionalRuleLabels on every alert" "storage storage storage" "$(yq -N "$pr | .spec.groups[].rules[].labels.team" "$mc" | xargs)"
  check "rule severity wins over additionalRuleLabels" "critical warning critical" \
    "$(yq -N "$pr | .spec.groups[].rules[].labels.severity" "$mc" | xargs)"
  check "runbookBaseUrl (trailing slash trimmed)" "https://runbooks.example.com/smb/SmbMountHealerControlPlaneAbsent.md" \
    "$(yq -N "$pr | .spec.groups[].rules[] | select(.alert == \"SmbMountHealerControlPlaneAbsent\") | .annotations.runbook_url" "$TMP/pr-custom.yaml")"

  if render "$TMP/bad.yaml" -f "$CHART/ci/monitoring-values.yaml" --api-versions "$PODMONITOR_API" --api-versions "$PROMETHEUSRULE_API" \
    --set 'monitoring.prometheusRule.disabledAlerts={SmbMountHealerNope}' 2>"$TMP/bad.err"; then
    fail "unknown alert in disabledAlerts was accepted"
  else
    check "unknown alert in disabledAlerts rejected" "1" "$(grep -c 'unknown alert "SmbMountHealerNope"' "$TMP/bad.err")"
  fi

  echo "assertions: digest"
  local digest
  digest=$(yq '.image.digest' "$CHART/ci/digest-values.yaml")
  check "image repo:tag@digest" "registry.example.com/smb-mount-healer:1.2.3@$digest registry.example.com/smb-mount-healer:1.2.3@$digest" \
    "$(yq -N 'select(.kind == "Deployment" or .kind == "DaemonSet") | .spec.template.spec.containers[0].image' "$TMP/digest.yaml" | xargs)"
  local ref="ghcr.io/marcherschel/smb-mount-healer:9.9.9@sha256:$(printf 'f%.0s' {1..64})"
  render "$TMP/reference.yaml" -f "$CHART/ci/digest-values.yaml" --set "image.reference=$ref"
  check "image.reference wins" "$ref $ref" \
    "$(yq -N 'select(.kind == "Deployment" or .kind == "DaemonSet") | .spec.template.spec.containers[0].image' "$TMP/reference.yaml" | xargs)"

  echo "assertions: invalid values are rejected"
  local bad
  for bad in \
    "controlplane.remediation.scaleDownWait=abc" \
    "controlplane.remediaton.scaleDownWait=1m" \
    "worker.probing.probeConcurrency=0" \
    "worker.probing.recheckAttempts=0" \
    "logging.level=verbose" \
    "image.digest=sha256:nope" \
    "quarantineDir=/srv/quarantine" \
    "metrics.podMonitor.enabled=true" \
    "monitoring.prometheusRule.enabled=true" \
    "monitoring.grafanaDashboard.labels.grafana_dashboard=1"; do
    if render "$TMP/bad.yaml" --set "$bad" 2>"$TMP/bad.err"; then
      fail "--set $bad was accepted"
    else
      pass "--set $bad rejected ($(grep -m1 -oE '(at |- at |Error: ).{0,80}' "$TMP/bad.err" | head -1))"
    fi
  done
}

case "${1:-}" in
  lint) cmd_lint ;;
  test) cmd_test ;;
  *)
    echo "usage: $0 lint|test" >&2
    exit 2
    ;;
esac

if [ "$failures" -gt 0 ]; then
  echo "chart-test $1: $failures failure(s)" >&2
  exit 1
fi
echo "chart-test $1: ok"
