#!/usr/bin/env bash

# Explicit proof phases keep long native cases measurable without changing
# their entry points, format matrix, failure injections, or assertions.
CASE_PHASE_NAME=""
CASE_PHASE_STARTED=""

case_phase_end() {
    local status=${1:-0} result=0
    [ -n "$CASE_PHASE_NAME" ] || return 0
    python3 - "$OUT/phases.jsonl" "$CASE_PHASE_NAME" "$CASE_PHASE_STARTED" "$status" <<'PY_PHASE' || result=$?
import json, pathlib, sys, time
path, phase, started, status = sys.argv[1:]
elapsed = (time.monotonic_ns() - int(started)) / 1e9
if elapsed < 0:
    raise SystemExit("phase clock moved backwards")
record = {"schema": 1, "phase": phase, "wall_seconds": elapsed, "exit_code": int(status)}
with pathlib.Path(path).open("a") as stream:
    stream.write(json.dumps(record, sort_keys=True) + "\n")
print("==> case phase " + json.dumps(record, sort_keys=True), flush=True)
PY_PHASE
    CASE_PHASE_NAME=""
    CASE_PHASE_STARTED=""
    return "$result"
}

case_phase_begin() {
    case_phase_end 0 || return
    CASE_PHASE_NAME=$1
    CASE_PHASE_STARTED=$(python3 -c 'import time; print(time.monotonic_ns())')
}
