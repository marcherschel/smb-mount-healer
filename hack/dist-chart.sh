#!/usr/bin/env bash
# Builds the chart release assets into dist/chart (run by `make dist-chart`,
# used by .github/workflows/release-chart.yaml). The chart is packaged as
# committed: Chart.yaml `version` and `appVersion` are not stamped.
#
#   smb-mount-healer-<version>.tgz        chart package
#   smb-mount-healer-dashboard.json       Grafana dashboard (monitoring/dashboards)
#   smb-mount-healer.rules.yaml           Prometheus rule file (monitoring/alerts)
#   smb-mount-healer-prometheusrule.yaml  the same alerts as a PrometheusRule (no namespace: kubectl apply -n ...)
#   SHA256SUMS
#
# Environment:
#   CHART_VERSION  expected chart version (optional; release-chart.yaml passes
#                  the tag without "chart-v" and the build fails on a mismatch)
#   HELM           helm binary (default: helm)
set -euo pipefail

cd "$(dirname "$0")/.."

HELM=${HELM:-helm}
CHART=production/smb-mount-healer
OUT=dist/chart

version=$(yq '.version' "$CHART/Chart.yaml")
app_version=$(yq '.appVersion' "$CHART/Chart.yaml")
if [ -n "${CHART_VERSION:-}" ] && [ "${CHART_VERSION#chart-v}" != "$version" ]; then
  echo "dist-chart: tag version ${CHART_VERSION#chart-v} != Chart.yaml version $version" >&2
  exit 1
fi
image=$(yq '.annotations."artifacthub.io/images"' "$CHART/Chart.yaml" | yq '.[0].image')
if [ "${image##*:}" != "$app_version" ]; then
  echo "dist-chart: artifacthub.io/images tag ${image##*:} != appVersion $app_version" >&2
  exit 1
fi

rm -rf "$OUT"
mkdir -p "$OUT"

"$HELM" lint --strict --quiet "$CHART"
"$HELM" package "$CHART" --destination "$OUT" >/dev/null

cp monitoring/dashboards/smb-mount-healer.json "$OUT/smb-mount-healer-dashboard.json"
cp monitoring/alerts/smb-mount-healer.rules.yaml "$OUT/smb-mount-healer.rules.yaml"

"$HELM" template smb-mount-healer "$OUT/smb-mount-healer-$version.tgz" \
  --show-only templates/monitoring/prometheusrule.yaml \
  --set monitoring.prometheusRule.enabled=true \
  --api-versions monitoring.coreos.com/v1/PrometheusRule |
  yq 'del(.metadata.namespace) | del(.metadata.labels."helm.sh/chart") | del(.metadata.labels."app.kubernetes.io/managed-by")' \
    >"$OUT/smb-mount-healer-prometheusrule.yaml"

(cd "$OUT" && sha256sum -- * >SHA256SUMS)
echo "dist-chart: chart $version (appVersion $app_version): $(ls "$OUT" | xargs)"
