#!/usr/bin/env bash
# Explicit CAP_SYS_ADMIN test entry; ordinary go test never elevates itself.
# Compile as the caller, then run only the ENOSPC case in a private mount namespace.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
privileged=()
if [ "$(id -u)" -ne 0 ]; then privileged=(sudo -n --); fi
"${privileged[@]}" true
work="$(mktemp -d "${TMPDIR:-/var/tmp}/sandboxer-enospc-test-XXXXXX")"
cleanup() {
    local status=$?
    trap - EXIT
    # The child may have timed out with root-owned files in this exact directory.
    "${privileged[@]}" rm -rf -- "$work" || { [ "$status" -ne 0 ] || status=1; }
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "==> build isolated vhost ENOSPC test binary"
(cd "$repo_root" && CGO_ENABLED=0 go test -c -o "$work/vhost.test" ./pkg/vhost)
parent_mnt="$(readlink /proc/self/ns/mnt)"
echo "==> run isolated vhost ENOSPC test as root"
"${privileged[@]}" timeout --kill-after=5s 30s \
    unshare --mount --propagation private \
    env "TMPDIR=$work" \
    "SANDBOXER_TMPFS_ENOSPC_ROOT=1" \
    "SANDBOXER_TMPFS_TEST_PARENT_MNT=$parent_mnt" \
    "$work/vhost.test" -test.run='^TestDiffTmpfsENOSPC$' -test.v \
    -test.count=1 -test.timeout=25s | tee "$work/result.log"

# A renamed, filtered-out or skipped test must not satisfy the required gate.
for name in TestDiffTmpfsENOSPC TestDiffTmpfsENOSPC/false TestDiffTmpfsENOSPC/true; do
    grep -Eq "^[[:space:]]*--- PASS: $name \\(" "$work/result.log" || {
        echo "required tmpfs ENOSPC case did not pass: $name" >&2
        exit 1
    }
done
if grep -Eq -- '--- SKIP: TestDiffTmpfsENOSPC' "$work/result.log"; then
    echo 'required tmpfs ENOSPC coverage was skipped' >&2
    exit 1
fi
