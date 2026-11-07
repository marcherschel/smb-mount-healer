#!/usr/bin/env bash
# Runs the e2e suite (go test -v) with a progress line after every top-level
# test, so a 30-50 min run shows how far along it is:
#
#   hack/go-test.sh [extra go test flags...]
#
#   --- PASS: TestS2TransientBlip (69.47s)
#   === PROGRESS 15/20 (75%): 15 passed, 0 failed, 0 skipped, elapsed 31m12s
#
# The total is the number of top-level tests selected by -run/-skip (from
# `go test -list`). Used by `make e2e-run` and the Tiltfile's e2e-tests
# resource; both tee the output to $E2E_LOG_DIR/<cluster>.go-test.log.
# The exit code is go test's (pipefail).
set -euo pipefail

cd "$(dirname "$0")/../../.."   # repository root
PKGS=./test/e2e/...

run_re=. skip_re=
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
    case ${args[i]} in
        -run|--run)   run_re=${args[i + 1]:-.} ;;
        -run=*|--run=*)   run_re=${args[i]#*=} ;;
        -skip|--skip) skip_re=${args[i + 1]:-} ;;
        -skip=*|--skip=*) skip_re=${args[i]#*=} ;;
    esac
done

# -list matches top-level names only; a -run with a subtest part (TestA/sub)
# is listed by its first element.
tests=$(go test -tags e2e -list "${run_re%%/*}" "${PKGS}" | grep -E '^Test' || true)
if [ -n "${skip_re}" ] && [ -n "${tests}" ]; then
    tests=$(grep -Ev -- "${skip_re%%/*}" <<<"${tests}" || true)
fi
total=$(grep -c . <<<"${tests}" || true)
echo "=== PROGRESS 0/${total} (0%): ${total} top-level tests selected"

go test -tags e2e "${PKGS}" -p 1 -count=1 -timeout 50m -v "$@" 2>&1 | {
    done_n=0 passed=0 failed=0 skipped=0
    while IFS= read -r line; do
        printf '%s\n' "${line}"
        # Top-level results start in column 0; subtests are indented.
        case ${line} in
            '--- PASS: Test'*) passed=$((passed + 1)) ;;
            '--- FAIL: Test'*) failed=$((failed + 1)) ;;
            '--- SKIP: Test'*) skipped=$((skipped + 1)) ;;
            *) continue ;;
        esac
        done_n=$((done_n + 1))
        pct=0
        if [ "${total}" -gt 0 ]; then pct=$((done_n * 100 / total)); fi
        printf '=== PROGRESS %d/%d (%d%%): %d passed, %d failed, %d skipped, elapsed %dm%02ds\n' \
            "${done_n}" "${total}" "${pct}" "${passed}" "${failed}" "${skipped}" \
            $((SECONDS / 60)) $((SECONDS % 60))
    done
}
