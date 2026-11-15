#!/usr/bin/env bash
# Runs the e2e suite (go test -v) one top-level test at a time, in a fixed
# order (version sort of the test names: S1, S2, S3, S3…Netem, S4 … S12,
# S12a, S12b, S13 …), independent of file names or declaration order. Each
# test is its own `go test -run '^Name$'` (the build is cached after the
# first), so a test that hangs hits its own timeout and the rest still run.
# A progress line follows every test:
#
#   hack/go-test.sh [extra go test flags...]
#
#   --- PASS: TestS2TransientBlip (69.47s)
#   === PROGRESS 15/20 (75%): 15 passed, 0 failed, 0 skipped, elapsed 31m12s
#
# -run/-skip select the tests (matched against top-level names with
# `go test -list`); all other flags are passed to every go test invocation.
# E2E_TEST_TIMEOUT is the per-test go test -timeout (default 20m).
# E2E_MAX_BASELINE_FAILURES (default 2, 0 = never): stop the suite once that
# many tests in a row could not get the cluster back to baseline (the
# framework prints "=== BASELINE NOT REACHED:"). A cluster that does not
# settle again fails every later test the same way, 10m each, until the
# job timeout cancels the run without a summary; the tests left are
# reported as not run.
# If E2E_ARTIFACTS and E2E_CLUSTER are set (CI), a failed test is followed by
# `hack/ci-e2e.sh collect` into $E2E_ARTIFACTS/tests/<Test>: kernel log, cifs
# state and healer logs as of that failure, not as of the end of the suite
# (a worker replaced by a later test takes its log with it).
# Used by `make e2e-run` and the Tiltfile's e2e-tests resource; both tee the
# output to $E2E_LOG_DIR/<cluster>.go-test.log.
# Exits non-zero if any test failed (or if none was selected).
set -euo pipefail

cd "$(dirname "$0")/../../.."   # repository root
PKGS=./test/e2e/...
TIMEOUT=${E2E_TEST_TIMEOUT:-20m}
MAX_BASELINE_FAILURES=${E2E_MAX_BASELINE_FAILURES:-2}
# Must match framework.BaselineNotReachedMarker.
BASELINE_MARKER='=== BASELINE NOT REACHED:'

run_re=. skip_re=
pass=()
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
    case ${args[i]} in
        -run|--run)   run_re=${args[i + 1]:-.}; i=$((i + 1)) ;;
        -run=*|--run=*)   run_re=${args[i]#*=} ;;
        -skip|--skip) skip_re=${args[i + 1]:-}; i=$((i + 1)) ;;
        -skip=*|--skip=*) skip_re=${args[i]#*=} ;;
        *) pass+=("${args[i]}") ;;
    esac
done

# -list matches top-level names only; a -run with a subtest part (TestA/sub)
# is listed by its first element.
tests=$(go test -tags e2e -list "${run_re%%/*}" "${PKGS}" | grep -E '^Test' || true)
if [ -n "${skip_re}" ] && [ -n "${tests}" ]; then
    tests=$(grep -Ev -- "${skip_re%%/*}" <<<"${tests}" || true)
fi
tests=$(LC_ALL=C sort -V <<<"${tests}" | grep . || true)
total=$(grep -c . <<<"${tests}" || true)
echo "=== PROGRESS 0/${total} (0%): ${total} top-level tests selected, in this order:"
sed 's/^/===   /' <<<"${tests}"
if [ "${total}" -eq 0 ]; then
    echo "no tests selected (-run '${run_re}' -skip '${skip_re}')" >&2
    exit 1
fi

done_n=0 passed=0 failed=0 skipped=0 failed_names=() not_run=()
baseline_streak=0 stopped=
while IFS= read -r t; do
    if [ -n "${stopped}" ]; then
        not_run+=("${t}")
        continue
    fi
    # A subtest part of -run (TestA/sub) is kept for its test.
    sub=
    case ${run_re} in */*) sub=/${run_re#*/} ;; esac
    rc=0
    go test -tags e2e "${PKGS}" -p 1 -count=1 -timeout "${TIMEOUT}" -v \
        ${pass[@]+"${pass[@]}"} -run "^${t}\$${sub}" \
        ${skip_re:+-skip "${skip_re}"} 2>&1 | {
        result=none baseline=
        while IFS= read -r line; do
            printf '%s\n' "${line}"
            case ${line} in
                "--- PASS: ${t} "*) result=pass ;;
                "--- FAIL: ${t} "*) result=fail ;;
                "--- SKIP: ${t} "*) result=skip ;;
                "${BASELINE_MARKER}"*) baseline=1 ;;
            esac
        done
        # A panic or timeout kills the binary before it prints --- FAIL.
        # 4: failed, and the baseline was not reached.
        case ${result} in pass) exit 0 ;; skip) exit 3 ;; *) exit $((baseline ? 4 : 1)) ;; esac
    } || rc=$?
    if [ "${rc}" -eq 4 ]; then
        baseline_streak=$((baseline_streak + 1))
    else
        baseline_streak=0
    fi
    case ${rc} in
        0) passed=$((passed + 1)) ;;
        3) skipped=$((skipped + 1)) ;;
        *) failed=$((failed + 1)); failed_names+=("${t}")
           if [ -n "${E2E_ARTIFACTS:-}" ] && [ -n "${E2E_CLUSTER:-}" ]; then
               hack/ci-e2e.sh collect "${E2E_CLUSTER}" "${E2E_ARTIFACTS}/tests/${t}" 2>&1 \
                   | sed 's/^/=== COLLECT: /' || true
           fi ;;
    esac
    done_n=$((done_n + 1))
    printf '=== PROGRESS %d/%d (%d%%): %d passed, %d failed, %d skipped, elapsed %dm%02ds\n' \
        "${done_n}" "${total}" $((done_n * 100 / total)) "${passed}" "${failed}" "${skipped}" \
        $((SECONDS / 60)) $((SECONDS % 60))
    if [ "${MAX_BASELINE_FAILURES}" -gt 0 ] && [ "${baseline_streak}" -ge "${MAX_BASELINE_FAILURES}" ]; then
        stopped=1
        echo "=== STOPPING: ${baseline_streak} tests in a row could not get the cluster back to baseline (E2E_MAX_BASELINE_FAILURES=${MAX_BASELINE_FAILURES})"
    fi
done <<<"${tests}"

if [ "${#not_run[@]}" -gt 0 ]; then
    echo "=== NOT RUN (${#not_run[@]}/${total}): ${not_run[*]}"
fi
if [ "${failed}" -gt 0 ] || [ "${#not_run[@]}" -gt 0 ]; then
    echo "=== FAILED (${failed}/${total}): ${failed_names[*]}"
    exit 1
fi
echo "=== OK: ${passed} passed, ${skipped} skipped"
