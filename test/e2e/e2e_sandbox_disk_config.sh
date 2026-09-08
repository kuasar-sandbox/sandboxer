#!/usr/bin/env bash
# Retired disk-size inputs must fail before artifact or VM side effects.
# Binary-only distributions exercise the CLI matrix; source-workspace runs
# additionally execute the production configuration and capacity regressions.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN_DIR="${BIN_DIR:-$ROOT/bin}"
CTL="$BIN_DIR/sandbox-ctl"
[[ -x "$CTL" ]] || { echo "sandbox-ctl not found: $CTL" >&2; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The standard BMS source workspace contains the candidate module. Do not
# require a Go toolchain or source checkout for the assembled binary e2e suite.
SOURCE=""
if command -v go >/dev/null 2>&1; then
  SOURCE="$(GOPROXY=off GOSUMDB=off go list -m -f '{{.Dir}}' github.com/kuasar-sandbox/sandboxer 2>/dev/null || true)"
fi
if [[ -n "$SOURCE" && -f "$SOURCE/pkg/config/disk_size_test.go" ]]; then
  (
    cd "$SOURCE"
    go test -count=1 -timeout=2m ./pkg/config
    go test -count=1 -timeout=2m ./pkg/sandbox -run '^TestPrepareDiff'
  )
else
  echo "Source-only Go regressions unavailable; running binary CLI coverage."
fi

cases=0
for shape in root root-overlay disk disk-overlay; do
  case "$shape" in
    root) field='boot.root'; format='boot: {root: {%s: %s}}\n' ;;
    root-overlay) field='boot.root.overlay'; format='boot: {root: {overlay: {%s: %s}}}\n' ;;
    disk) field='boot.disks[0]'; format='boot: {disks: [{name: data, %s: %s}]}\n' ;;
    disk-overlay) field='boot.disks[0].overlay'; format='boot: {disks: [{name: data, overlay: {%s: %s}}]}\n' ;;
  esac
  for key in diff_size size; do
    for value in 512MiB '""' null; do
      printf "$format" "$key" "$value" > "$WORK/input.yaml"
      for mode in cold from restore; do
        extra=()
        case "$mode" in
          from) extra=(--from "file://$WORK/missing.sandbox") ;;
          restore) extra=(--restore "file://$WORK/missing.snapshot") ;;
        esac
        if timeout 10s "$CTL" run --config "$WORK/input.yaml" \
          --run-root "$WORK/run" --base-root "$WORK/base" \
          "${extra[@]}" > "$WORK/output" 2>&1; then
          echo "Accepted $field.$key=$value in $mode mode" >&2
          exit 1
        fi
        if ! grep -Fq "$field.$key is not supported" "$WORK/output" || \
           ! grep -Fq 'required logical capacity' "$WORK/output"; then
          echo "Missing migration error for $field.$key=$value in $mode mode" >&2
          cat "$WORK/output" >&2
          exit 1
        fi
        [[ ! -e "$WORK/run" && ! -e "$WORK/base" ]] || {
          echo "Invalid disk-size input created runtime state" >&2
          exit 1
        }
        cases=$((cases + 1))
      done
    done
  done
done
echo "PASS: $cases retired disk-size CLI cases rejected before runtime side effects."
