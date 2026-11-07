#!/usr/bin/env bash
# Preflight for the smb-mount-healer e2e cluster (first Tilt resource).
#
#   hack/preflight.sh <k3d-cluster-name>
#
# Checks, from inside every k3d node container (docker exec):
#   1. cifs is usable by the kernel: listed in /proc/filesystems, or loadable
#      on demand (a probe mount against a closed port triggers the kernel's
#      module autoload; ENODEV means the module is not available on the host).
#   2. Mount propagation: the mount containing /var/lib/kubelet is shared in
#      the node's mountinfo (k3d's K3D_FIX_MOUNTS entrypoint makes / rshared).
#      Bidirectional hostPath mounts (worker, csi-smb-node, chaos) need it.
# Optional (informational): sch_netem for the tc-netem variant of S3.
set -euo pipefail

cluster=${1:?usage: preflight.sh <k3d-cluster-name>}
fail=0
err() { echo "PREFLIGHT FAIL: $*" >&2; fail=1; }

command -v docker >/dev/null || { echo "PREFLIGHT FAIL: docker CLI not found" >&2; exit 1; }

mapfile -t nodes < <(docker ps --filter "label=k3d.cluster=${cluster}" --filter "label=k3d.role=server" --format '{{.Names}}'
                     docker ps --filter "label=k3d.cluster=${cluster}" --filter "label=k3d.role=agent" --format '{{.Names}}')
if [ "${#nodes[@]}" -lt 2 ]; then
    echo "PREFLIGHT FAIL: expected a running server and agent for k3d cluster '${cluster}', found: ${nodes[*]:-none}." >&2
    echo "Create it with: make e2e-up E2E_CLUSTER=${cluster}" >&2
    exit 1
fi

# Optional fields of a mountinfo line: field 7 up to the "-" separator.
propagation_of() { # <node> <path>: prints the optional fields of the mount containing <path>
    docker exec "$1" cat /proc/self/mountinfo | awk -v p="$2" '
        {
            mp = $5; opt = ""
            for (i = 7; i <= NF && $i != "-"; i++) opt = opt " " $i
            if (p == mp || mp == "/" || index(p, mp "/") == 1) {
                if (length(mp) >= length(best)) { best = mp; bestopt = opt }
            }
        }
        END { printf "%s|%s\n", best, bestopt }'
}

for node in "${nodes[@]}"; do
    echo "== ${node}"

    # --- mount propagation -------------------------------------------------
    res=$(propagation_of "${node}" /var/lib/kubelet)
    mp=${res%%|*}; opts=${res#*|}
    if [[ "${opts}" == *shared:* ]]; then
        echo "   propagation: /var/lib/kubelet is under '${mp}' (${opts# }) - ok"
    else
        err "${node}: mount '${mp}' containing /var/lib/kubelet is not shared (optional fields: '${opts# }').
       Bidirectional mount propagation will not work. k3d's mounts fix must be
       enabled (K3D_FIX_MOUNTS unset or =1 when running 'k3d cluster create');
       recreate the cluster (make e2e-down e2e-up)."
    fi

    # --- cifs ----------------------------------------------------------------
    if docker exec "${node}" grep -qw cifs /proc/filesystems; then
        echo "   cifs: in /proc/filesystems - ok"
        continue
    fi
    # Probe mount: nothing listens on 127.0.0.1:1, so a working cifs module
    # fails with a connection error, a missing one with ENODEV
    # ("No such device"). The attempt also autoloads the module if present.
    out=$(docker exec "${node}" sh -c 'd=$(mktemp -d) && mount -t cifs //127.0.0.1/preflight "$d" -o port=1,guest,vers=3.0 2>&1; rc=$?; umount "$d" 2>/dev/null; rmdir "$d"; exit $rc' 2>&1) || true
    if docker exec "${node}" grep -qw cifs /proc/filesystems; then
        echo "   cifs: loaded on demand by a probe mount - ok"
    else
        err "${node}: the kernel has no cifs filesystem (probe mount said: ${out:-<nothing>}).
       cifs lives in the HOST kernel; the e2e cluster cannot work without it.
       Load it on the docker host / VM: 'sudo modprobe cifs' (a devcontainer
       cannot modprobe; do it on the VM that runs dockerd)."
    fi
done

# sch_netem: informational only, the netem variant of S3 skips without it.
if docker exec "${nodes[0]}" sh -c 'grep -qw sch_netem /proc/modules 2>/dev/null'; then
    echo "info: sch_netem loaded (S3 netem variant possible)"
else
    echo "info: sch_netem not loaded; it may autoload on first 'tc qdisc add ... netem', otherwise S3's netem variant is skipped"
fi

if [ "${fail}" -ne 0 ]; then
    echo "PREFLIGHT FAILED for k3d cluster '${cluster}' - see messages above." >&2
    exit 1
fi
echo "preflight ok for k3d cluster '${cluster}'"
