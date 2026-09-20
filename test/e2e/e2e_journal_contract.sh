#!/usr/bin/env bash
# Source-workspace regressions complement the native/KVM journal lifecycle case.
# Packaged suites still validate the shipped CLI without requiring Go sources.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${BIN:-$ROOT/bin}"
CTL="$BIN/sandbox-ctl"
[[ -x "$CTL" ]] || { echo "sandbox-ctl not found: $CTL" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

"$CTL" --help >"$WORK/help"
grep -Fq -- '--log-to default|journald=TAG[,FIELD=VALUE...]' "$WORK/help"
for target in 'journald=app,MESSAGE=override' 'journald=app,A=1,A=2' 'journald=app,A=%' 'journald=app,'; do
    status=0
    "$CTL" run --log-to "$target" --run-root "$WORK/run" --base-root "$WORK/base" \
        >"$WORK/out" 2>"$WORK/err" || status=$?
    [[ "$status" = 2 && ! -e "$WORK/run" && ! -e "$WORK/base" ]] || {
        echo "invalid journal target was not rejected before runtime setup: $target ($status)" >&2
        cat "$WORK/err" >&2
        exit 1
    }
done

# Source-wide checks are scheduled once by run_all.sh; keep this case contract-focused.

echo "==> e2e_journal_contract: OK"
