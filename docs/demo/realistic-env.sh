# shellcheck shell=bash
# Fake kubectl for docs/demo/realistic.tape. Nothing talks to a cluster.
# The output follows a real recording of the healer on a k3d cluster:
# watch rows per status write (repeats, an empty first STAGE, stages that a
# single reconcile chains are skipped), real AGE progression, and kubectl's
# column widths growing as longer values arrive. Names are made up.

export PS1=$'\[\e[38;5;39m\]~/homelab\[\e[0m\] \[\e[38;5;245m\]❯\[\e[0m\] '
STATE=broken
SMBR=worker-1-1794096731

kubectl() {
  case "$*" in
    "-n media exec deploy/media-server -- ls /media")
      if [[ $STATE == broken ]]; then
        sleep 1.4
        echo "ls: /media: Host is down"
        echo "command terminated with exit code 1"
        return 1
      fi
      sleep 0.3
      echo "movies  music  photos  tv" ;;

    "get smbmounts")
      sleep 0.35
      if [[ $STATE == broken ]]; then
        echo "NAME                     NODE       PV              HEALTH    REASON       STAGE   LAST OUTCOME   AGE"
        echo "worker-1.media-config    worker-1   media-config    Suspect   stat_error                          41d"
        echo "worker-1.media-library   worker-1   media-library   Suspect   stat_error                          41d"
      else
        echo "NAME                     NODE       PV              HEALTH    REASON   STAGE   LAST OUTCOME   AGE"
        echo "worker-1.media-config    worker-1   media-config    Healthy                    healed         41d"
        echo "worker-1.media-library   worker-1   media-library   Healthy                    healed         41d"
      fi ;;

    "get smbr -w --watch-only")
      # delay(s, already compressed ~6x) | stage | outcome | age
      local rows=(
        "4.0||| 0s" "0|planning|| 0s" "0|planning|| 0s"
        "1.6|draining||15s" "0|draining||15s" "0|draining||15s"
        "0.8|releasing_mounts||20s" "0.8|releasing_mounts||25s"
        "3.5|awaiting_unstage||55s" "1.6|awaiting_unstage||65s" "0.8|awaiting_unstage||70s"
        "1.6|quarantining||80s" "0|quarantining||80s" "0|awaiting_unstage_completion||80s"
        "1.6|restarting_driver||90s" "0.2|scaling_up||91s"
        "0.2|verifying||91s" "0|verifying||91s" "0|verifying||91s"
        "1.6|verifying||100s" "0.4|done|healed|100s" "0|done|healed|100s"
      )
      local ws=8 wo=10 r d st out age header=1
      for r in "${rows[@]}"; do
        IFS='|' read -r d st out age <<<"$r"; age=${age# }
        sleep "$d"
        (( ${#st} + 3 > ws )) && ws=$(( ${#st} + 3 ))
        if (( header )); then
          printf '%-22s%-11s%-11s%-*s%-*s%s\n' NAME NODE TYPE "$ws" STAGE "$wo" OUTCOME AGE
          header=0
        fi
        printf '%-22s%-11s%-11s%-*s%-*s%s\n' "$SMBR" worker-1 Workload "$ws" "$st" "$wo" "$out" "$age"
      done
      STATE=healed
      sleep 60 ;;  # VHS sends Ctrl+C

    "-n media get pods")
      sleep 0.35
      echo "NAME                            READY   STATUS    RESTARTS   AGE"
      echo "media-server-74648bdfcd-tc9zg   1/1     Running   0          22s" ;;

    *) command kubectl "$@" ;;
  esac
}
clear
