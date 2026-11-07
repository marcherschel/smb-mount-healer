#!/usr/bin/env bash
# Fails if the Grafana dashboard or the Prometheus alerts of the healer use a
# smb_mount_healer_* metric name that internal/metrics/names.go does not define.
# Histogram series (_bucket, _count, _sum) of a defined name are allowed.
#
# Names with a longer prefix (e.g. foo_smb_mount_healer_*) are ignored.
#
# Usage: hack/check-dashboard-metrics.sh   (from anywhere in the repo)
set -euo pipefail

PROJECT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

NAMES_GO="${PROJECT_DIR}/internal/metrics/names.go"
SOURCES=(
    "${PROJECT_DIR}/monitoring/dashboards/smb-mount-healer.json"
    "${PROJECT_DIR}/monitoring/alerts/smb-mount-healer.rules.yaml"
)

for f in "${NAMES_GO}" "${SOURCES[@]}"; do
    if [ ! -f "${f}" ]; then
        echo "missing file: ${f}" >&2
        exit 1
    fi
done

prefix=$(sed -nE 's/^const Namespace = "([a-z_]+)"$/\1/p' "${NAMES_GO}")
if [ "${prefix}" != "smb_mount_healer" ]; then
    echo "cannot read the metric prefix from ${NAMES_GO} (got '${prefix}')" >&2
    exit 1
fi

# Every `Namespace + "_xyz"` in names.go is a metric name.
mapfile -t defined < <(grep -oE 'Namespace \+ "_[a-z0-9_]+"' "${NAMES_GO}" \
    | sed -E "s/Namespace \\+ \"_([a-z0-9_]+)\"/${prefix}_\\1/" | sort -u)
if [ "${#defined[@]}" -lt 10 ]; then
    echo "found only ${#defined[@]} metric names in ${NAMES_GO}; parser out of date?" >&2
    exit 1
fi

is_defined() {
    local name=$1 d
    for d in "${defined[@]}"; do
        [ "${name}" = "${d}" ] && return 0
    done
    return 1
}

fail=0
checked=0
for src in "${SOURCES[@]}"; do
    # (^|[^a-z0-9_]) skips names with a longer prefix.
    mapfile -t used < <(grep -oE "(^|[^a-z0-9_])${prefix}_[a-z0-9_]+" "${src}" \
        | sed -E "s/^[^a-z0-9_]//" | sort -u)
    for name in "${used[@]}"; do
        [ -n "${name}" ] || continue
        checked=$((checked + 1))
        base=${name}
        for suffix in _bucket _count _sum; do
            if [ "${name%"${suffix}"}" != "${name}" ] && is_defined "${name%"${suffix}"}"; then
                base=${name%"${suffix}"}
                break
            fi
        done
        if ! is_defined "${base}"; then
            echo "${src#"${PROJECT_DIR}"/}: ${name} is not defined in internal/metrics/names.go" >&2
            fail=1
        fi
    done
done

if [ "${fail}" -ne 0 ]; then
    exit 1
fi
echo "ok: ${checked} metric reference(s) in ${#SOURCES[@]} file(s) match internal/metrics/names.go (${#defined[@]} names)"
