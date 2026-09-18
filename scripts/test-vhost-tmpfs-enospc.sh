#!/usr/bin/env bash
# Build unprivileged; explicitly run only the capability-requiring test as root.
# No unprivileged user namespace or host security configuration is needed.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for tool in go unshare timeout readlink grep; do
    command -v "$tool" >/dev/null || { echo "missing test prerequisite: $tool" >&2; exit 1; }
done
privileged=()
if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null || { echo 'explicit root test invocation requires sudo' >&2; exit 1; }
    privileged=(sudo -n --)
fi
work="$(mktemp -d "${TMPDIR:-/var/tmp}/sandboxer-enospc-test-XXXXXX")"
cleanup() {
    # A forcibly terminated root test can leave root-owned entries in this
    # invocation's private directory; never clean any shared mount/path.
    rm -rf -- "$work" 2>/dev/null || "${privileged[@]}" rm -rf -- "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo '==> build isolated vhost ENOSPC test binary'
(cd "$repo_root" && CGO_ENABLED=0 go test -c -o "$work/vhost.test" ./pkg/vhost)
parent_mnt="$(readlink /proc/self/ns/mnt)"
command=(timeout --kill-after=5s 35s unshare --mount --propagation private env
    "TMPDIR=$work" SANDBOXER_TMPFS_ENOSPC_ROOT=1 "SANDBOXER_TMPFS_TEST_PARENT_MNT=$parent_mnt"
    "$work/vhost.test" -test.run='^TestDiffTmpfsENOSPC$' -test.timeout=30s -test.v)
echo '==> run both isolated vhost ENOSPC cases as root'
"${privileged[@]}" "${command[@]}" > "$work/result.log" 2>&1 || {
    status=$?; cat "$work/result.log"; exit "$status";
}
cat "$work/result.log"
for test_name in TestDiffTmpfsENOSPC TestDiffTmpfsENOSPC/false TestDiffTmpfsENOSPC/true; do
    grep -Eq "^[[:space:]]*--- PASS: ${test_name} \\(" "$work/result.log" || {
        echo "required ENOSPC case did not execute successfully: $test_name" >&2
        exit 1
    }
done
if grep -Eq -- '--- SKIP: TestDiffTmpfsENOSPC' "$work/result.log"; then
    echo 'required ENOSPC coverage was skipped' >&2
    exit 1
fi
