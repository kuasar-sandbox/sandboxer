#!/usr/bin/env bash
# Offline launch-contract checks; no real elevation, namespace or mount occurs.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/scratch"
export TMPDIR="$work/scratch" TEST_TRACE="$work/trace" TEST_RESULT=pass TEST_UID=0
export PATH="$work/bin:$PATH"
cat > "$work/bin/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = "test -c -o $4 ./pkg/vhost" ] && [ "$CGO_ENABLED" = 0 ]
printf 'go\n' >> "$TEST_TRACE"
cat > "$4" <<'BIN'
#!/usr/bin/env bash
set -euo pipefail
[ "$SANDBOXER_TMPFS_ENOSPC_ROOT" = 1 ]
[ -n "$SANDBOXER_TMPFS_TEST_PARENT_MNT" ]
[ "$*" = '-test.run=^TestDiffTmpfsENOSPC$ -test.timeout=30s -test.v' ]
case "$TEST_RESULT" in
    empty) exit 0 ;;
    skipped) echo '--- SKIP: TestDiffTmpfsENOSPC (0.00s)'; exit 0 ;;
    failure) exit 7 ;;
    timeout) exit 124 ;;
esac
printf '%s\n' '--- PASS: TestDiffTmpfsENOSPC (0.00s)' '    --- PASS: TestDiffTmpfsENOSPC/false (0.00s)'
[ "$TEST_RESULT" = missing-encrypted ] || echo '    --- PASS: TestDiffTmpfsENOSPC/true (0.00s)'
BIN
chmod +x "$4"
SH
cat > "$work/bin/id" <<'SH'
#!/usr/bin/env bash
[ "$*" = -u ] || exit 2
printf '%s\n' "$TEST_UID"
SH
cat > "$work/bin/sudo" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = -n ] && [ "$2" = -- ]
printf 'sudo\n' >> "$TEST_TRACE"
[ "$TEST_RESULT" != denied ] || exit 1
shift 2
exec "$@"
SH
cat > "$work/bin/timeout" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --kill-after=5s ] && [ "$2" = 35s ]
printf 'timeout\n' >> "$TEST_TRACE"
shift 2
exec "$@"
SH
cat > "$work/bin/unshare" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = --mount ] && [ "$2" = --propagation ] && [ "$3" = private ]
printf 'unshare\n' >> "$TEST_TRACE"
shift 3
exec "$@"
SH
chmod +x "$work/bin/"*
for spec in 'pass 0 0' 'pass 1001 0' 'empty 0 1' 'skipped 0 1' 'missing-encrypted 0 1' 'failure 0 7' 'timeout 0 124' 'denied 1001 1'; do
    read -r TEST_RESULT TEST_UID expected <<< "$spec"
    export TEST_RESULT TEST_UID
    : > "$TEST_TRACE"
    rc=0
    bash "$root/scripts/test-vhost-tmpfs-enospc.sh" > "$work/output" 2>&1 || rc=$?
    if [ "$rc" -ne "$expected" ]; then
        cat "$work/output" >&2
        echo "runner case $spec returned $rc" >&2
        exit 1
    fi
    if [ -n "$(find "$work/scratch" -mindepth 1 -print -quit)" ]; then
        echo "runner case $spec leaked owned scratch" >&2; exit 1
    fi
    if [ "$TEST_UID" = 0 ]; then
        if grep -Fxq sudo "$TEST_TRACE"; then echo "unexpected elevation in root path" >&2; exit 1; fi
    else
        grep -Fxq sudo "$TEST_TRACE"
    fi
    echo "PASS: isolated ENOSPC runner $spec"
done
# The full source suite must invoke the real runner, not just its offline tests.
grep -Fq './scripts/test-vhost-tmpfs-enospc.sh' "$root/scripts/ci-source-checks.sh"
echo 'test-vhost-tmpfs-runner: 8 offline launch cases passed (real ENOSPC not exercised here)'
