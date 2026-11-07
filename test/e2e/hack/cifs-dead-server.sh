#!/usr/bin/env bash
# Kernel CIFS client diagnostic (written for S8/S3 on the hosted runner,
# kernel 6.17-azure): does the client answer stat of a share root from
# cache after the server is gone, and does statfs still reach the server?
# And how long after a 20s smbd freeze do stat / statfs keep failing
# when probed the way the healer does (KILL after 5s, 1s apart)?
#
#   test/e2e/hack/cifs-dead-server.sh
#
# Runs against the e2e cluster left behind by `make e2e-run` (KUBECONFIG):
# mounts samba-landmine's share in the chaos pod on the agent node (a private
# mount in the pod's own namespace, invisible to the healer), freezes smbd
# for 20s while probing, then scales samba-landmine to 0 and logs stat /
# stat -f of the mount root every 5s.
# Always restores samba-landmine and unmounts. Informational: never fails.
set -uo pipefail

ns_chaos=smbmh-chaos
ns_samba=smbmh-samba
unc=//samba-landmine.${ns_samba}.svc.cluster.local/landmine
mnt=/tmp/cifs-dead-server
rounds=${ROUNDS:-24}

node=$(kubectl get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[0].metadata.name}')
pod=$(kubectl -n "$ns_chaos" get pods -l app.kubernetes.io/name=chaos --field-selector "spec.nodeName=${node}" \
    -o jsonpath='{.items[0].metadata.name}')
if [ -z "$node" ] || [ -z "$pod" ]; then
    echo "cifs-dead-server: no agent node or chaos pod found, skipping"
    exit 0
fi
user=$(kubectl -n kube-system get secret storagebox-e2e-credentials -o jsonpath='{.data.username}' | base64 -d)
pass=$(kubectl -n kube-system get secret storagebox-e2e-credentials -o jsonpath='{.data.password}' | base64 -d)

cx() { kubectl -n "$ns_chaos" exec "$pod" -c chaos -- sh -c "$1"; }
scale() { kubectl -n "$ns_samba" scale deploy/samba-landmine --replicas="$1"; }
restore() {
    echo "== restore"
    scale 1
    cx "umount -f -l $mnt 2>/dev/null; rmdir $mnt 2>/dev/null" || true
}
trap restore EXIT

echo "== node ${node}, chaos pod ${pod}"
scale 1
kubectl -n "$ns_samba" rollout status deploy/samba-landmine --timeout=180s
cx "echo kernel: \$(uname -r); for p in dir_cache_timeout enable_oplocks; do \
    echo cifs.\$p=\$(cat /sys/module/cifs/parameters/\$p 2>/dev/null || echo n/a); done"
# Same options as the e2e fixtures (framework.MountOptions). The password goes
# in through the environment so it never shows up in the log.
kubectl -n "$ns_chaos" exec "$pod" -c chaos -- env PASSWD="$pass" sh -c \
    "mkdir -p $mnt && mount -t cifs '$unc' $mnt -o 'username=${user},uid=1000,gid=1000,file_mode=0775,dir_mode=0775,vers=3.0,cache=strict,noserverino'" ||
    { echo "cifs-dead-server: mount failed, skipping"; exit 0; }

# One exec for the whole loop so the timings are the node's, not kubectl's.
probe_loop=$(cat <<EOF
start=\$(date +%s)
for i in \$(seq 1 \$1); do
    t=\$(( \$(date +%s) - start ))
    s=\$(timeout -s KILL 10 stat -c 'ino=%i mtime=%Y' $mnt 2>&1); src=\$?
    f=\$(timeout -s KILL 10 stat -f -c 'type=%T blocks=%b free=%f' $mnt 2>&1); frc=\$?
    printf '%4ss  stat rc=%-3s %-40s | statfs rc=%-3s %s\n' "\$t" "\$src" "\$s" "\$frc" "\$f"
    sleep 5
done
EOF
)

echo "== server up"
cx "set -- 2; $probe_loop"

# Healer-style probing through a 20s SIGSTOP of smbd: stat then statfs, each
# killed after 5s, 1s between rounds. Shows the recovery tail after SIGCONT.
freeze_loop=$(cat <<EOF
start=\$(date +%s)
while [ \$(( \$(date +%s) - start )) -lt 75 ]; do
    t=\$(( \$(date +%s) - start ))
    s=\$(timeout -s KILL 5 stat -c 'ino=%i' $mnt 2>&1); src=\$?
    f=\$(timeout -s KILL 5 stat -f -c 'free=%f' $mnt 2>&1); frc=\$?
    printf '%4ss  stat rc=%-3s %-12s | statfs rc=%-3s %s\n' "\$t" "\$src" "\$s" "\$frc" "\$f"
    sleep 1
done
EOF
)
echo "== freezing smbd for 20s (rc 137 = killed after 5s)"
lpod=$(kubectl -n "$ns_samba" get pods -l app.kubernetes.io/name=samba-landmine -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$ns_samba" exec "$lpod" -c samba -- sh -c \
    "pkill -STOP smbd && (setsid sh -c 'sleep 20; pkill -CONT smbd' </dev/null >/dev/null 2>&1 &)"
cx "$freeze_loop"
cx "dmesg 2>/dev/null | grep -i cifs | tail -40" || true

echo "== scaling samba-landmine to 0"
scale 0
kubectl -n "$ns_samba" wait --for=delete pod -l app.kubernetes.io/name=samba-landmine --timeout=120s || true
echo "== server gone (rc 137 = killed after 10s)"
cx "set -- $rounds; $probe_loop"

echo "== /proc/fs/cifs/DebugData"
cx "cat /proc/fs/cifs/DebugData 2>/dev/null | head -60" || true
exit 0
