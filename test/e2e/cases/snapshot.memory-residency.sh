#!/usr/bin/env bash
# Non-merged memory parents remain explicit while local disks merge; restored page cache stays resident.
set -euo pipefail

source "${E2E_LIB:?E2E_LIB is required}/common.sh"
SANDBOXER_LIB="$E2E_LIB/sandboxer"
source "$SANDBOXER_LIB/tarstream.sh"

: "${E2E_WORKSPACE:?E2E_WORKSPACE is required}"
: "${WORK:?WORK is required}"
: "${OUT:?OUT is required}"
: "${E2E_IMAGE:?E2E_IMAGE is required}"

require_root
require_kvm
for command in docker ip mkfs.ext4 python3 sha256sum timeout truncate; do
    require_command "$command"
done
for binary in sandbox-ctl flatten-ctl cloud-hypervisor mkfs.erofs; do
    require_binary "$binary"
done
for file in sandbox-init sandbox-runtime.bundle vmlinux; do
    [ -f "$BIN/$file" ] || e2e_fail "missing prepared product: $file"
done
[ -r "$SANDBOXER_LIB/tarstream.sh" ] || e2e_fail "missing prepared tarstream helpers"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 || e2e_fail "prepared E2E_IMAGE is not loaded: $E2E_IMAGE"

mkdir -p "$WORK" "$OUT" "$WORK/runtime" "$WORK/base" "$WORK/checkpoint"
TAP_NAME="mr$((BASHPID % 1000000))"
TAP_CREATED=0
PIDS=()
HIDDEN_PARENT=""
PARENT_SIBLING=""
cleanup() {
    local status=$?
    set +e
    for pid in "${PIDS[@]}"; do
        kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null
        wait "$pid" 2>/dev/null
    done
    if [ -n "$HIDDEN_PARENT" ] && [ -e "$HIDDEN_PARENT" ] && [ -n "$PARENT_SIBLING" ] && [ ! -e "$PARENT_SIBLING" ]; then
        mv "$HIDDEN_PARENT" "$PARENT_SIBLING"
    fi
    [ "$TAP_CREATED" = 1 ] && ip link del "$TAP_NAME" 2>/dev/null
    exit "$status"
}
trap cleanup EXIT

ip tuntap add dev "$TAP_NAME" mode tap
TAP_CREATED=1
ip addr add 169.254.1.0/31 dev "$TAP_NAME"
ip link set "$TAP_NAME" up

ROOT_IMAGE="$WORK/root.img"
docker save "$E2E_IMAGE" | "$BIN/flatten-ctl" export --output "$ROOT_IMAGE" --no-progress
[ -s "$ROOT_IMAGE" ] || e2e_fail "flatten produced an empty root artifact"
mkdir -p "$WORK/dataset-source"
printf 'DATASET-OK\n' >"$WORK/dataset-source/DATASET-OK"
MKFS_EROFS_PATH="$BIN/mkfs.erofs" "$BIN/flatten-ctl" export --no-progress \
    --tmpdir "$WORK/tmp" --output "$WORK/dataset.img" "$WORK/dataset-source"
ROOT_REF="$(plaintext_tarstream_ref "$ROOT_IMAGE")"
DATASET_REF="$(plaintext_tarstream_ref "$WORK/dataset.img")"
truncate -s 512M "$WORK/root.ext4"; mkfs.ext4 -q -F -O ^has_journal "$WORK/root.ext4"
truncate -s 256M "$WORK/scratch.ext4"; mkfs.ext4 -q -F -O ^has_journal "$WORK/scratch.ext4"
truncate -s 256M "$WORK/dataset.ext4"; mkfs.ext4 -q -F -O ^has_journal "$WORK/dataset.ext4"

cat >"$WORK/cold.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: memory-residency }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  cmdline: "console=hvc0 printk.time=1"
  root:
    base: $ROOT_REF
    overlay: { diff: file://$WORK/root.ext4 }
  disks:
    - { name: scratch, diff_template: file://$WORK/scratch.ext4 }
    - { name: dataset, base: $DATASET_REF, overlay: { diff_template: file://$WORK/dataset.ext4 } }
mounts:
  - { target: /scratch, type: disk, source: scratch }
  - { target: /data, type: disk, source: dataset }
launch: { exec: /bin/sleep, args: ["3600"], restart: never }
EOF

wait_exec() {
    local sid=$1 pid=$2
    for _ in $(seq 1 90); do
        if timeout -k 2s 6 "$BIN/sandbox-ctl" exec --sandbox-id "$sid" --run-root "$WORK/runtime" -- /bin/true >/dev/null 2>&1; then
            return 0
        fi
        kill -0 "$pid" 2>/dev/null || return 1
        sleep 1
    done
    return 1
}

MINCORE_PROBE='import ctypes,mmap,os,sys
path=sys.argv[1]
with open(path,"rb",buffering=0) as source:
 size=os.fstat(source.fileno()).st_size
 assert size>0
 mapping=mmap.mmap(source.fileno(),size,access=mmap.ACCESS_COPY)
 try:
  view=(ctypes.c_char*size).from_buffer(mapping)
  try:
   page=os.sysconf("SC_PAGE_SIZE"); pages=(size+page-1)//page
   vec=(ctypes.c_ubyte*pages)(); libc=ctypes.CDLL(None,use_errno=True)
   fn=libc.mincore; fn.argtypes=(ctypes.c_void_p,ctypes.c_size_t,ctypes.POINTER(ctypes.c_ubyte)); fn.restype=ctypes.c_int
   if fn(ctypes.addressof(view),size,vec)!=0: raise OSError(ctypes.get_errno(),os.strerror(ctypes.get_errno()),path)
   resident=sum(v&1 for v in vec); print(f"mincore resident={resident}/{pages} path={path}")
   if resident!=pages: raise SystemExit(f"not fully resident: {resident}/{pages}")
  finally: del view
 finally: mapping.close()'

SID="memory-residency-$BASHPID"
LOG1="$OUT/cold.log"
timeout -k 10s 120 "$BIN/sandbox-ctl" run --config "$WORK/cold.yaml" --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" >"$LOG1" 2>&1 &
PID1=$!; PIDS+=("$PID1")
wait_exec "$SID" "$PID1" || e2e_fail "cold multi-disk sandbox did not become executable"
WARM_PATH=/scratch/resident.bin
WARM_BYTES=$((16 * 1024 * 1024))
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c \
    'python3 - "$1" "$2" <<"PY"
import hashlib,sys
size=int(sys.argv[1]); path=sys.argv[2]; pattern=b"KUASAR-RESIDENT-SET\n"
h=hashlib.sha256()
with open(path,"wb") as out:
 remaining=size
 while remaining:
  chunk=(pattern*((min(remaining,1<<20)+len(pattern)-1)//len(pattern)))[:min(remaining,1<<20)]
  out.write(chunk); h.update(chunk); remaining-=len(chunk)
print(h.hexdigest())
PY
sync' sh "$WARM_BYTES" "$WARM_PATH" >"$WORK/warm-create.out"
WARM_SHA="$(grep -Eo '[0-9a-f]{64}' "$WORK/warm-create.out" | tail -1)"
[ "${#WARM_SHA}" = 64 ] || e2e_fail "guest warm file digest is invalid"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c \
    'echo ROOT-S-OK > /root-s; echo SCRATCH-S-OK > /scratch/persist; echo DATA-S-OK > /data/persist; sync'

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$WORK/checkpoint" --run-root "$WORK/runtime" >"$OUT/s1.log" 2>&1
wait "$PID1" || true; PIDS=()
S1="$WORK/checkpoint/$SID.snapshot"
[ -f "$S1" ] || e2e_fail "parent snapshot is missing"
PARENT_MEMORY_ARTIFACT="$(readlink -f "$S1")"
[ -f "$PARENT_MEMORY_ARTIFACT" ] || e2e_fail "parent memory target is missing"
PARENT_MEMORY_BASENAME="$(basename "$PARENT_MEMORY_ARTIFACT")"
"$BIN/sandbox-ctl" info --json "$S1" >"$WORK/s1-s.json"
S1_E_REF="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["SandboxRef"])' "$WORK/s1-s.json")"
S1_E_BASENAME="$(python3 -c 'import json,os,sys; print(os.path.basename(json.load(open(sys.argv[1]))["SandboxRef"].split("@",1)[0]))' "$WORK/s1-s.json")"

truncate -s 512M "$WORK/r1-root.ext4"
truncate -s 256M "$WORK/r1-data.ext4"
cat >"$WORK/restore1.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: memory-residency-r1 }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff: file://$WORK/r1-root.ext4 } }
  disks:
    - { name: scratch }
    - { name: dataset, overlay: { diff: file://$WORK/r1-data.ext4 } }
EOF
LOG2="$OUT/restore-parent.log"
timeout -k 10s 120 "$BIN/sandbox-ctl" run --restore "$S1" --config "$WORK/restore1.yaml" --sandbox-id "$SID" \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" >"$LOG2" 2>&1 &
PID2=$!; PIDS+=("$PID2")
wait_exec "$SID" "$PID2" || e2e_fail "parent restore did not become executable"

# Establish a deterministic guest page cache and prove residency before capture.
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c \
    'sync; echo 3 > /proc/sys/vm/drop_caches; cat "$1" >/dev/null' sh "$WARM_PATH"
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- python3 -c "$MINCORE_PROBE" "$WARM_PATH" >"$WORK/warm-before.out"
assert_contains "$WORK/warm-before.out" 'mincore resident='
"$BIN/sandbox-ctl" exec --sandbox-id "$SID" --run-root "$WORK/runtime" -- /bin/sh -c \
    'echo ROOT-W-OK > /root-w; echo SCRATCH-W-OK > /scratch/working-set; echo DATA-W-OK > /data/working-set; sync'

"$BIN/sandbox-ctl" snapshot --sandbox-id "$SID" --output "$WORK/checkpoint" --run-root "$WORK/runtime" \
    --drop-caches=false --merge-ref=false >"$OUT/w.log" 2>&1
wait "$PID2" || true; PIDS=()
grep -Fq 'quiesce: guest acked (drop_caches=skipped' "$LOG2" || e2e_fail "capture did not preserve guest page cache"
W="$WORK/checkpoint/$SID.snapshot"
[ -f "$W" ] || e2e_fail "working snapshot is missing"
[ -f "$WORK/checkpoint/$PARENT_MEMORY_BASENAME" ] || e2e_fail "non-merged local memory parent is not retained as a sibling"
"$BIN/sandbox-ctl" info --json "$W" >"$WORK/w-s.json"
W_E_BASENAME="$(python3 -c 'import json,os,sys; print(os.path.basename(json.load(open(sys.argv[1]))["SandboxRef"].split("@",1)[0]))' "$WORK/w-s.json")"
"$BIN/sandbox-ctl" info --json "$WORK/checkpoint/$S1_E_BASENAME" >"$WORK/s1-e.json"
"$BIN/sandbox-ctl" info --json "$WORK/checkpoint/$W_E_BASENAME" >"$WORK/w-e.json"
python3 - "$WORK/s1-e.json" "$WORK/w-e.json" "$WORK/w-s.json" "$PARENT_MEMORY_BASENAME" "$S1_E_REF" <<'PY'
import json,os,sys
parent=json.load(open(sys.argv[1])); working=json.load(open(sys.argv[2])); snap=json.load(open(sys.argv[3]))
refs=snap.get('FromRefs') or []
assert len(refs)==1 and os.path.basename(refs[0].split('@',1)[0])==sys.argv[4], refs

def nodes(doc):
 boot=doc['Boot']; return [boot['Root'], *(boot.get('Disks') or [])]
def top(node):
 overlay=node.get('Overlay'); return overlay['Base'] if overlay else node['Base']
def chain(node):
 overlay=node.get('Overlay'); return (overlay.get('BaseFromRefs') if overlay else node.get('BaseFromRefs')) or []
parent_nodes=nodes(parent); working_nodes=nodes(working)
assert len(parent_nodes)==3 and len(working_nodes)==3
for index,(p,w) in enumerate(zip(parent_nodes,working_nodes)):
 ptop=top(p)
 if index==0 and ptop=='self': ptop=sys.argv[5]
 assert top(w)!=ptop and ptop not in chain(w), (index,ptop,w)
PY

# The memory lower is mandatory. Hide it recoverably and prove the restore graph
# fails before the VMM wrapper can execute.
PARENT_SIBLING="$WORK/checkpoint/$PARENT_MEMORY_BASENAME"
HIDDEN_PARENT="$WORK/$PARENT_MEMORY_BASENAME.hidden"
mv "$PARENT_SIBLING" "$HIDDEN_PARENT"
VMM_MARKER="$WORK/vmm-started"
CH_WRAPPER="$WORK/ch-marker"
cat >"$CH_WRAPPER" <<EOF
#!/bin/sh
: > "$VMM_MARKER"
exec "$BIN/cloud-hypervisor" "\$@"
EOF
chmod +x "$CH_WRAPPER"
truncate -s 512M "$WORK/r2-root.ext4"
truncate -s 256M "$WORK/r2-data.ext4"
cat >"$WORK/restore2.yaml" <<EOF
resources: { capacity: { cpu: 1, memory: 512MiB }, allocatable: { cpu: 1, memory: 512MiB } }
network: { tap: $TAP_NAME, interface: eth0, ip: 169.254.1.1/31, hostname: memory-residency-r2 }
boot:
  kernel: file://$BIN/vmlinux
  runtime: file://$BIN/sandbox-runtime.bundle
  root: { overlay: { diff: file://$WORK/r2-root.ext4 } }
  disks:
    - { name: scratch }
    - { name: dataset, overlay: { diff: file://$WORK/r2-data.ext4 } }
EOF
set +e
timeout -k 5s 30 "$BIN/sandbox-ctl" run --restore "$W" --config "$WORK/restore2.yaml" --sandbox-id missing-memory-lower \
    --ch-binary "$CH_WRAPPER" --run-root "$WORK/runtime" >"$OUT/missing-lower.log" 2>&1
MISSING_RC=$?
set -e
[ "$MISSING_RC" -ne 0 ] || e2e_fail "restore accepted a missing local memory lower"
grep -Fq 'from_refs[0]' "$OUT/missing-lower.log" || e2e_fail "missing-lower error did not identify from_refs[0]"
grep -Fq "$PARENT_MEMORY_BASENAME" "$OUT/missing-lower.log" || e2e_fail "missing-lower error did not identify the missing sibling"
[ ! -e "$VMM_MARKER" ] || e2e_fail "VMM started before missing memory lower rejection"
mv "$HIDDEN_PARENT" "$PARENT_SIBLING"; HIDDEN_PARENT=""

LOG3="$OUT/restore-working.log"
timeout -k 10s 120 "$BIN/sandbox-ctl" run --restore "$W" --config "$WORK/restore2.yaml" --sandbox-id memory-residency-w \
    --ch-binary "$BIN/cloud-hypervisor" --run-root "$WORK/runtime" --base-root "$WORK/base" >"$LOG3" 2>&1 &
PID3=$!; PIDS+=("$PID3")
wait_exec memory-residency-w "$PID3" || e2e_fail "working snapshot restore did not become executable"
# mincore must pass before the first content read in the restored guest.
"$BIN/sandbox-ctl" exec --sandbox-id memory-residency-w --run-root "$WORK/runtime" -- python3 -c "$MINCORE_PROBE" "$WARM_PATH" >"$WORK/warm-after.out"
assert_contains "$WORK/warm-after.out" 'mincore resident='
"$BIN/sandbox-ctl" exec --sandbox-id memory-residency-w --run-root "$WORK/runtime" -- sha256sum "$WARM_PATH" >"$WORK/warm-hash.out"
grep -Fq "$WARM_SHA  $WARM_PATH" "$WORK/warm-hash.out" || e2e_fail "restored warm file digest changed"
"$BIN/sandbox-ctl" exec --sandbox-id memory-residency-w --run-root "$WORK/runtime" -- /bin/sh -c \
    'set -e; grep -qx ROOT-S-OK /root-s; grep -qx SCRATCH-S-OK /scratch/persist; grep -qx DATA-S-OK /data/persist; grep -qx ROOT-W-OK /root-w; grep -qx SCRATCH-W-OK /scratch/working-set; grep -qx DATA-W-OK /data/working-set; grep -qx DATASET-OK /data/DATASET-OK'

kill -TERM "$PID3"; wait "$PID3" || true; PIDS=()

echo "PASS snapshot.memory-residency.sh"
