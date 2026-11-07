#!/usr/bin/env bash
# CI helpers for the e2e job in .github/workflows/smb-mount-healer.yaml.
# The job runs in a container with the host's Docker socket mounted; k3d
# creates the cluster as sibling containers on the docker host.
#
#   hack/ci-e2e.sh connect    <cluster>         join this container to k3d-<cluster>
#   hack/ci-e2e.sh kubeconfig <cluster>         point $KUBECONFIG at k3d-<cluster>-server-0:6443
#   hack/ci-e2e.sh collect    <cluster> <dir>   dump CRs, events, logs, node mountinfo into <dir>
#   hack/ci-e2e.sh cleanup    <cluster>         leave the k3d network, remove tilt-built images
#
# The cluster itself is created/deleted with make e2e-up / e2e-down.
set -euo pipefail

HEALER_NS=smb-mount-healer-system

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
    for node in $(docker ps -a --filter "label=k3d.cluster=${cluster}" --format '{{.Names}}'); do
        docker exec "${node}" cat /proc/self/mountinfo >"${dir}/nodes/${node}.mountinfo" 2>&1
        docker exec "${node}" sh -c 'grep -E " - (cifs|smb3) " /proc/self/mountinfo' \
            >"${dir}/nodes/${node}.cifs-mounts" 2>&1
        docker logs --timestamps "${node}" >"${dir}/nodes/${node}.k3s.log" 2>&1
    done
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
    collect)    cmd_collect "$@" ;;
    cleanup)    cmd_cleanup "$@" ;;
    *) echo "usage: $0 {connect|kubeconfig|collect|cleanup} <cluster> [dir]" >&2; exit 2 ;;
esac
