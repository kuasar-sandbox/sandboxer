#!/usr/bin/env bash
# Shipped sandbox-ctl CLI/config rejection contracts. No source tree or VM.
set -euo pipefail
source "${E2E_LIB:?E2E_LIB is required}/common.sh"
require_binary sandbox-ctl
CTL="$BIN/sandbox-ctl"
WORK="${WORK:?WORK is required}"
mkdir -p "$WORK"

"$CTL" --help >"$WORK/help"
assert_contains "$WORK/help" '--log-to default|journald=TAG[,FIELD=VALUE...]'

for target in 'journald=app,MESSAGE=override' 'journald=app,A=1,A=2' 'journald=app,A=%' 'journald=app,'; do
    status=0
    "$CTL" run --log-to "$target" --run-root "$WORK/run" --base-root "$WORK/base"         >"$WORK/out" 2>"$WORK/err" || status=$?
    [ "$status" = 2 ] && [ ! -e "$WORK/run" ] && [ ! -e "$WORK/base" ] ||
        e2e_fail "invalid journal target was not rejected before runtime setup: $target (exit=$status)"
done

cases=0
for shape in root root-overlay disk disk-overlay root-block root-overlay-block disk-block disk-overlay-block; do
    case "$shape" in
      root) field='boot.root'; format='boot: {root: {%s: %s}}\n' ;;
      root-overlay) field='boot.root.overlay'; format='boot: {root: {overlay: {%s: %s}}}\n' ;;
      disk) field='boot.disks[0]'; format='boot: {disks: [{name: data, %s: %s}]}\n' ;;
      disk-overlay) field='boot.disks[0].overlay'; format='boot: {disks: [{name: data, overlay: {%s: %s}}]}\n' ;;
      root-block) field='boot.root'; format='boot:\n  root:\n    %s: %s\n' ;;
      root-overlay-block) field='boot.root.overlay'; format='boot:\n  root:\n    overlay:\n      %s: %s\n' ;;
      disk-block) field='boot.disks[0]'; format='boot:\n  disks:\n    - name: data\n      %s: %s\n' ;;
      disk-overlay-block) field='boot.disks[0].overlay'; format='boot:\n  disks:\n    - name: data\n      overlay:\n        %s: %s\n' ;;
    esac
    for key in diff_size size; do
      for value in 512MiB '""' null; do
        printf "$format" "$key" "$value" >"$WORK/input.yaml"
        for mode in cold from restore; do
          extra=()
          case "$mode" in
            from) extra=(--from "file://$WORK/missing.sandbox") ;;
            restore) extra=(--restore "file://$WORK/missing.snapshot") ;;
          esac
          status=0
          timeout 10s "$CTL" run --config "$WORK/input.yaml"             --run-root "$WORK/run" --base-root "$WORK/base" "${extra[@]}"             >"$WORK/output" 2>&1 || status=$?
          [ "$status" -ne 0 ] || e2e_fail "accepted retired $field.$key=$value in $mode mode"
          assert_contains "$WORK/output" "$field.$key is not supported"
          assert_contains "$WORK/output" 'required logical capacity'
          [ ! -e "$WORK/run" ] && [ ! -e "$WORK/base" ] ||
            e2e_fail "retired disk-size input created runtime state"
          cases=$((cases + 1))
        done
      done
    done
done

echo "PASS basic.sandbox-cli.sh ($cases retired disk-size forms plus journal target contract)"
