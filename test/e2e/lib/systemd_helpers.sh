#!/usr/bin/env bash

# A private derivative of the already prepared image makes auto fallback
# deterministic. Candidate removal runs inside its own stopped-after-setup
# container, so image symlinks cannot redirect host filesystem writes.
systemd_prepare_root_fixture() {
    : "${E2E_IMAGE:?prepared E2E_IMAGE is required}"
    : "${WORK:?WORK is required}"
    : "${OUT:?OUT is required}"
    : "${BIN:?BIN is required}"
    mkdir -p "$WORK" "$OUT"
    docker image inspect "$E2E_IMAGE" --format '{{.Id}}' >"$OUT/systemd-source-image-id"
    SYSTEMD_FIXTURE_CONTAINER="$(docker create --pull=never --network none --user 0:0 \
        --entrypoint /bin/sh "$E2E_IMAGE" -c '
        set -eu
        for path in /usr/lib/systemd/systemd /lib/systemd/systemd /sbin/init; do
            rm -f -- "$path"
            test ! -e "$path"
            test ! -L "$path"
        done
        echo SYSTEMD_NO_CANDIDATES
    ')"
    docker start --attach "$SYSTEMD_FIXTURE_CONTAINER" >"$OUT/systemd-fixture.log" 2>&1
    [ "$(docker inspect --format '{{.State.ExitCode}}' "$SYSTEMD_FIXTURE_CONTAINER")" = 0 ] || \
        e2e_fail "systemd fixture candidate removal failed"
    grep -qx SYSTEMD_NO_CANDIDATES "$OUT/systemd-fixture.log" || \
        e2e_fail "systemd fixture did not confirm candidate absence"
    docker commit "$SYSTEMD_FIXTURE_CONTAINER" >"$OUT/systemd-fixture-commit.stdout"
    SYSTEMD_FIXTURE_IMAGE="$(cat "$OUT/systemd-fixture-commit.stdout")"
    [[ "$SYSTEMD_FIXTURE_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || \
        e2e_fail "systemd fixture commit did not return exactly one image ID"
    docker image inspect "$SYSTEMD_FIXTURE_IMAGE" --format '{{.Id}}' >"$OUT/systemd-fixture-image-id"
    docker save "$SYSTEMD_FIXTURE_IMAGE" | \
        "$BIN/flatten-ctl" export --output "$WORK/systemd-root.img" --no-progress
    [ -s "$WORK/systemd-root.img" ] || e2e_fail "systemd fixture root artifact is empty"
    SYSTEMD_BASE_REF="$(plaintext_tarstream_ref "$WORK/systemd-root.img")"
    SYSTEMD_DIFF_TEMPLATE="$WORK/systemd-diff-template.ext4"
    truncate -s 1G "$SYSTEMD_DIFF_TEMPLATE"
    mkfs.ext4 -q -F -O ^has_journal "$SYSTEMD_DIFF_TEMPLATE"
}

systemd_cleanup_root_fixture() {
    if [ -n "${SYSTEMD_FIXTURE_CONTAINER:-}" ]; then
        docker rm -f "$SYSTEMD_FIXTURE_CONTAINER" >/dev/null 2>&1 || true
        SYSTEMD_FIXTURE_CONTAINER=""
    fi
    if [ -n "${SYSTEMD_FIXTURE_IMAGE:-}" ]; then
        docker image rm "$SYSTEMD_FIXTURE_IMAGE" >/dev/null 2>&1 || true
        SYSTEMD_FIXTURE_IMAGE=""
    fi
}
