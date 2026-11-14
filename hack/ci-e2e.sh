#!/usr/bin/env bash
# CI helpers for the e2e job in .github/workflows/smb-mount-healer.yaml.
# The job runs in a container with the host's Docker socket mounted; k3d
# creates the cluster as sibling containers on the docker host.
#
#   hack/ci-e2e.sh connect    <cluster>         join this container to k3d-<cluster>
#   hack/ci-e2e.sh kubeconfig <cluster>         point $KUBECONFIG at k3d-<cluster>-server-0:6443
#   hack/ci-e2e.sh info       <cluster>         print runner VM, kernel and cifs module details
#   hack/ci-e2e.sh collect    <cluster> <dir>   dump CRs, events, logs, node mountinfo, cifs state into <dir>
#   hack/ci-e2e.sh cleanup    <cluster>         leave the k3d network, remove tilt-built images
#
# The cluster itself is created/deleted with make e2e-up / e2e-down.
set -euo pipefail

HEALER_NS=smb-mount-healer-system
# Fallback for collect_dmesg; same image as the chaos DaemonSet.
# renovate: datasource=docker depName=alpine
DMESG_IMAGE=alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# Full id of the container this script runs in. Docker bind-mounts
# /var/lib/docker/containers/<id>/{hostname,hosts,resolv.conf} into every
# container, so the id shows up in our own mountinfo (works with cgroup v2,
# where /proc/self/cgroup is just "0::/"). Fall back to the hostname, which
# defaults to the short id unless the container was started with --hostname.
self_container() {
    local id
    id=$(grep -oE '/containers/[0-9a-f]{64}/(hostname|hosts|resolv\.conf)' /proc/self/mountinfo \
        | head -n1 | cut -d/ -f3 || true)
    if [ -z "${id}" ]; then
        id=$(hostname)
    fi
    if ! docker inspect --type container "${id}" >/dev/null 2>&1; then
        echo "cannot determine this job container's id (tried mountinfo and hostname '${id}')" >&2
        return 1
    fi
    printf '%s\n' "${id}"
}

cmd_connect() {
    local cluster=${1:?cluster} id net
    net="k3d-${cluster}"
    id=$(self_container)
    if docker inspect --type container -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "${id}" \
        | tr ' ' '\n' | grep -qxF "${net}"; then
        echo "${id:0:12} already on ${net}"
        return 0
    fi
    docker network connect "${net}" "${id}"
    echo "connected ${id:0:12} to ${net}"
}

# k3d.yaml disables the load balancer, so the kubeconfig points at the port the
# server publishes on the docker host (0.0.0.0:<random>), which is not this
# container's localhost. Use the server container's name on the k3d network
# instead (k3s puts the node hostname into the serving cert's SANs).
cmd_kubeconfig() {
    local cluster=${1:?cluster} ctx server
    : "${KUBECONFIG:?KUBECONFIG must be set}"
    ctx="k3d-${cluster}"
    server="https://k3d-${cluster}-server-0:6443"
    if ! kubectl config get-contexts "${ctx}" >/dev/null 2>&1; then
        k3d kubeconfig merge "${cluster}" --output "${KUBECONFIG}" >/dev/null
    fi
    kubectl config set-cluster "${ctx}" --server="${server}" >/dev/null
    kubectl config use-context "${ctx}" >/dev/null
    for i in $(seq 1 30); do
        if kubectl --context "${ctx}" get --raw /readyz >/dev/null 2>&1; then
            echo "API server reachable at ${server}"
            kubectl --context "${ctx}" get nodes -o wide
            return 0
        fi
        echo "waiting for ${server} (${i}/30)..."
        sleep 2
    done
    echo "API server ${server} not reachable from the job container" >&2
    kubectl --context "${ctx}" get --raw /readyz || true
    return 1
}

# Node containers of the cluster (servers, agents, registry).
cluster_nodes() {
    docker ps -a --filter "label=k3d.cluster=${1:?cluster}" --format '{{.Names}}'
}

# The job container shares the host kernel but not its view of kernel modules;
# read kernel and cifs details through a (privileged) node container.
cmd_info() {
    local cluster=${1:?cluster} node
    node=$(cluster_nodes "${cluster}" | grep -- '-server-0$' | head -n1 || true)
    echo "runner: ${RUNNER_NAME:-?} image ${ImageOS:-?} ${ImageVersion:-?}"
    echo "host nproc (job container): $(nproc)"
    grep -m1 'model name' /proc/cpuinfo || true
    grep -E '^(MemTotal|MemAvailable):' /proc/meminfo || true
    echo "kernel: $(uname -r)"
    if [ -n "${node}" ]; then
        echo "nproc in ${node}: $(docker exec "${node}" nproc)"
        echo "cifs module: $(docker exec "${node}" cat /sys/module/cifs/version 2>/dev/null || echo 'not loaded')"
        echo "sch_netem: $(docker exec "${node}" sh -c 'test -d /sys/module/sch_netem && echo loaded || echo "not loaded"')"
    fi
}

# The host kernel log. The k3s node image has no dmesg, so read it through the
# privileged chaos pod (util-linux dmesg, ISO timestamps). If that pod is not
# there (setup failed before it), fall back to a throwaway privileged busybox
# container on the docker host (timestamps in seconds since boot; uptime and
# date are printed first to convert them).
collect_dmesg() {
    local cluster=${1:?cluster} pod
    pod=$(kubectl --context "k3d-${cluster}" --request-timeout=30s -n smbmh-chaos \
        get pods -l app.kubernetes.io/name=chaos --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [ -n "${pod}" ] && kubectl --context "k3d-${cluster}" --request-timeout=60s -n smbmh-chaos \
        exec "${pod}" -- dmesg --time-format=iso; then
        return 0
    fi
    echo "# chaos pod unavailable (${pod:-none}); busybox dmesg via docker" >&2
    docker run --rm --privileged "${DMESG_IMAGE}" \
        sh -c 'echo "# date: $(date -u +%FT%TZ) uptime: $(cut -d" " -f1 /proc/uptime)s"; dmesg'
}

cmd_collect() {
    local cluster=${1:?cluster} dir=${2:?dir} ctx node pod
    ctx="k3d-${cluster}"
    mkdir -p "${dir}/logs" "${dir}/nodes"
    # Best effort: never fail the collection step on a single missing item.
    set +e
    local k=(kubectl --context "${ctx}" --request-timeout=30s)

    "${k[@]}" get nodes -o wide >"${dir}/nodes.txt" 2>&1
    "${k[@]}" get pods -A -o wide >"${dir}/pods.txt" 2>&1
    "${k[@]}" get smbm,smbr,smbt,smbq -o wide >"${dir}/crs.txt" 2>&1
    "${k[@]}" get smbm,smbr,smbt,smbq -o yaml >"${dir}/crs.yaml" 2>&1
    "${k[@]}" get events -A --sort-by=.lastTimestamp -o wide >"${dir}/events.txt" 2>&1
    "${k[@]}" get events -A -o yaml >"${dir}/events.yaml" 2>&1
    "${k[@]}" get deploy,sts,ds -A -o wide >"${dir}/workloads.txt" 2>&1
    "${k[@]}" get pv,pvc -A -o wide >"${dir}/volumes.txt" 2>&1
    "${k[@]}" get volumeattachments,csinodes -o yaml >"${dir}/csi.yaml" 2>&1

    # Healer (controlplane + worker), samba, chaos, fixtures: every pod,
    # current and previous container logs.
    for ns in "${HEALER_NS}" smbmh-samba smbmh-chaos smbmh-fixtures; do
        "${k[@]}" -n "${ns}" describe pods >"${dir}/logs/${ns}.describe.txt" 2>&1
        for pod in $("${k[@]}" -n "${ns}" get pods -o name 2>/dev/null); do
            "${k[@]}" -n "${ns}" logs "${pod}" --all-containers --prefix --timestamps \
                >"${dir}/logs/${ns}.${pod#pod/}.log" 2>&1
            "${k[@]}" -n "${ns}" logs "${pod}" --all-containers --prefix --timestamps --previous \
                >"${dir}/logs/${ns}.${pod#pod/}.previous.log" 2>/dev/null \
                || rm -f "${dir}/logs/${ns}.${pod#pod/}.previous.log"
        done
    done

    # csi-driver-smb (node plugin per node + controller).
    for sel in app=csi-smb-node app=csi-smb-controller; do
        for pod in $("${k[@]}" -n kube-system get pods -l "${sel}" -o name 2>/dev/null); do
            "${k[@]}" -n kube-system logs "${pod}" --all-containers --prefix --timestamps \
                >"${dir}/logs/kube-system.${pod#pod/}.log" 2>&1
            "${k[@]}" -n kube-system logs "${pod}" --all-containers --prefix --timestamps --previous \
                >"${dir}/logs/kube-system.${pod#pod/}.previous.log" 2>/dev/null \
                || rm -f "${dir}/logs/kube-system.${pod#pod/}.previous.log"
        done
    done

    # Node containers: mountinfo (globalmount/pod mount state) and k3s logs.
    for node in $(cluster_nodes "${cluster}"); do
        docker exec "${node}" cat /proc/self/mountinfo >"${dir}/nodes/${node}.mountinfo" 2>&1
        docker exec "${node}" sh -c 'grep -E " - (cifs|smb3) " /proc/self/mountinfo' \
            >"${dir}/nodes/${node}.cifs-mounts" 2>&1
        docker logs --timestamps "${node}" >"${dir}/nodes/${node}.k3s.log" 2>&1
    done

    # Host kernel state (shared by all node containers): the kernel log and the
    # CIFS client's sessions, tree connects, in-flight requests and credits.
    collect_dmesg "${cluster}" >"${dir}/dmesg.txt" 2>&1
    node=$(cluster_nodes "${cluster}" | grep -- '-server-0$' | head -n1 || true)
    if [ -n "${node}" ]; then
        docker exec "${node}" cat /proc/fs/cifs/DebugData >"${dir}/cifs-DebugData.txt" 2>&1
        docker exec "${node}" cat /proc/fs/cifs/Stats >"${dir}/cifs-Stats.txt" 2>&1
    fi
    cmd_info "${cluster}" >"${dir}/runner-info.txt" 2>&1
    set -e
    echo "collected diagnostics into ${dir}"
}

cmd_cleanup() {
    local cluster=${1:?cluster} id
    if id=$(self_container 2>/dev/null); then
        # Must leave the network before k3d deletes it.
        if docker network disconnect --force "k3d-${cluster}" "${id}" 2>/dev/null; then
            echo "disconnected ${id:0:12} from k3d-${cluster}"
        fi
    fi
    # SMBMH_E2E_IMAGE_MODE=k3d-import builds smb-mount-healer:tilt-* into the
    # host's docker; e2e runs are serialized (concurrency group), so these are
    # ours. Other images on the runner are left alone.
    docker image ls --filter 'reference=smb-mount-healer:tilt-*' -q | sort -u \
        | xargs -r docker image rm -f >/dev/null 2>&1 || true
}

sub=${1:-}
shift || true
case "${sub}" in
    connect)    cmd_connect "$@" ;;
    kubeconfig) cmd_kubeconfig "$@" ;;
    info)       cmd_info "$@" ;;
    collect)    cmd_collect "$@" ;;
    cleanup)    cmd_cleanup "$@" ;;
    *) echo "usage: $0 {connect|kubeconfig|info|collect|cleanup} <cluster> [dir]" >&2; exit 2 ;;
esac
