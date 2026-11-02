#!/usr/bin/env bash
# e2e-requires: ["docker","kvm","userfaultfd"]
# Current root/data disks survive image, E and S runs; Reset preserves later
# writes while immutable checkpoint A remains independently exportable.
set -euo pipefail
source "${E2E_LIB:?}/common.sh"
source "$E2E_LIB/sandboxer/tarstream.sh"
: "${WORK:?}" "${OUT:?}" "${E2E_IMAGE:?}"
require_root
require_kvm
for binary in sandbox-ctl flatten-ctl cloud-hypervisor; do require_binary "$binary"; done
mkdir -p "$WORK/run" "$WORK/base" "$OUT"
SID="reset-$BASHPID"
INSTANCE="instance-$BASHPID"
CP="$WORK/base/$SID/checkpoint"
PID=""
trap 'if [ -n "$PID" ]; then kill -TERM "$PID" 2>/dev/null || true; wait "$PID" || true; fi' EXIT
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$WORK/root.image" --no-progress
ROOT="$(plaintext_tarstream_ref "$WORK/root.image")"
for disk in root data; do
    truncate -s 256M "$WORK/$disk.ext4"
    mkfs.ext4 -q -F -O ^has_journal "$WORK/$disk.ext4"
done
cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root:
    base: $ROOT
    overlay: { diff_template: file://$WORK/root.ext4 }
  disks:
    - { name: data, diff_template: file://$WORK/data.ext4 }
mounts:
  - { target: /data, type: disk, source: data }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF
cat >"$WORK/host.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
EOF
guest() { "$BIN/sandbox-ctl" exec --path-id "$PATH_ID" --run-root "$WORK/run" -- "$@"; }
start() {
    local run=$1
    PATH_ID=$2
    shift 2
    timeout -k 10s 600 "$BIN/sandbox-ctl" run --sandbox-id "$SID" --path-id "$PATH_ID" \
        --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/run" --base-root "$WORK/base" \
        --enable-checkpoint --retain-instance "$INSTANCE" --run-id "$run" "$@" >"$OUT/$run.log" 2>&1 &
    PID=$!
    for _ in $(seq 1 90); do
        if timeout -k 2s 5 "$BIN/sandbox-ctl" exec --path-id "$PATH_ID" --run-root "$WORK/run" -- /bin/true >/dev/null 2>&1; then return; fi
        if ! kill -0 "$PID" 2>/dev/null; then tail -100 "$OUT/$run.log" >&2; e2e_fail "run $run exited"; fi
        sleep 1
    done
    e2e_fail "run $run readiness timeout"
}
stop() { kill -TERM "$PID" 2>/dev/null || true; wait "$PID" || true; PID=""; }
write_data() { guest /bin/sh -c "printf '%s' '$1' >/retained-data; printf '%s' '$1-data' >/data/retained-data; sync"; }
check_data() {
    guest /bin/sh -c 'cat /retained-data; printf "\n"; cat /data/retained-data; printf "\n"' >"$OUT/$1.actual"
    printf '%s\n%s-data\n' "$2" "$2" >"$OUT/$1.expected"
    cmp "$OUT/$1.actual" "$OUT/$1.expected"
}
result_path() {
    python3 - "$1" "$2" "$CP" <<'PY'
import json, pathlib, sys
ref = json.load(open(sys.argv[1]))[sys.argv[2]]
assert ref.startswith('file://'), ref
name = ref.removeprefix('file://').split('@', 1)[0]
assert name == pathlib.Path(name).name and name not in ('', '.', '..'), ref
print(pathlib.Path(sys.argv[3]) / name)
PY
}
reset() {
    "$BIN/sandbox-ctl" reset --checkpoint-dir "$CP" --sandbox-id "$SID" \
        --instance-id "$INSTANCE" --run-id "$1" --operation-id "reset-$1" --json >"$OUT/reset-$1.json"
    E="$(result_path "$OUT/reset-$1.json" sandbox_ref)"
    "$BIN/sandbox-ctl" reset --checkpoint-dir "$CP" --sandbox-id "$SID" \
        --instance-id "$INSTANCE" --run-id "$1" --operation-id "reset-$1" --json >"$OUT/reset-$1-replay.json"
    cmp "$OUT/reset-$1.json" "$OUT/reset-$1-replay.json"
}

# No pre-existing checkpoint: retain image lower before its original path goes.
start image "$SID" --config "$WORK/cold.yaml"
write_data initial
stop
rm "$WORK/root.image" "$WORK/root.ext4" "$WORK/data.ext4"
reset image
start cold-e "$SID" --config "$WORK/host.yaml" --from "file://$E"
check_data initial initial
write_data A
"$BIN/sandbox-ctl" export --path-id "$SID" --run-root "$WORK/run" --checkpoint user-A --resume --json >"$OUT/A.json"
A="$(result_path "$OUT/A.json" sandboxRef)"
write_data B
stop
reset cold-e
start latest "$SID" --config "$WORK/host.yaml" --from "file://$E"
check_data after-reset B
"$BIN/sandbox-ctl" snapshot --path-id "$SID" --run-root "$WORK/run" --checkpoint user-S --json >"$OUT/S.json"
wait "$PID" || true
PID=""
S="$(result_path "$OUT/S.json" snapshotRef)"
start memory-s "$SID" --config "$WORK/host.yaml" --restore "file://$S"
check_data from-memory B
write_data C
stop
reset memory-s
start final-e "$SID" --config "$WORK/host.yaml" --from "file://$E"
check_data final C
stop

# Export the old named artifact through the standard path, then actually boot it.
printf 'crypto: {ref: off}\n' >"$WORK/plain-ref.yaml"
"$BIN/sandbox-ctl" publish --manifest-config "$WORK/plain-ref.yaml" --to-ref-location "history=file://$WORK/history" --json "file://$A" >"$OUT/export-A.json"
HIST="$(python3 - "$OUT/export-A.json" <<'PY'
import json, sys
print(json.load(open(sys.argv[1]))['sandboxRef'])
PY
)"
start historical "$SID-history" --config "$WORK/host.yaml" --from "$HIST" --ref-location "history=file://$WORK/history"
check_data historical A
stop
python3 - "$CP/current-disks.json" "$SID" "$INSTANCE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
assert d['run'] == {'sandbox_id': sys.argv[2], 'instance_id': sys.argv[3], 'run_id': 'final-e'}, d['run']
assert [x['name'] for x in d['disks']] == ['', 'data']
PY
echo 'PASS: image/E/S current data, old checkpoint payload, offline replay and exact run binding'
