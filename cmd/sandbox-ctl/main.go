// sandbox-ctl is the host-side control tool for one sandbox lifecycle.
// Subcommands:
//
//	run       — explicit cold start, Sandbox E cold start, or Snapshot S restore
//	export    — create a runnable Sandbox E without memory state
//	snapshot  — create Sandbox E and memory Snapshot S at one freeze point
//	exec      — run an ad-hoc command inside a running sandbox
//	config    — produce / merge / validate a sandbox.yaml
//	info      — inspect a Sandbox E or Snapshot S
//	publish   — publish a complete E or S graph
//
// See docs/sandbox.md for the full design.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
)

func init() {
	// Microsecond precision so timing analysis (cold-start latency,
	// vhost handshake delta, vsock launch handshake) is computable
	// directly from log timestamps.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	// Make SIGPIPE non-fatal. Go's default disposition for SIGPIPE on
	// fd 1/2 is to terminate the process immediately. When sandbox-ctl
	// is run as `... | tee log`, Ctrl+C delivers SIGINT to the whole
	// foreground group: tee exits at once, and sandbox-ctl's next log
	// write to the now-readerless pipe would be killed by SIGPIPE
	// mid-shutdown — orphaning the CH child (leaked tap/run dir). With
	// SIGPIPE ignored those writes return EPIPE (dropped by log) and the
	// SIGINT-driven graceful CH teardown in pkg/sandbox runs to completion.
	signal.Ignore(syscall.SIGPIPE)
}

func main() {
	if len(os.Args) < 2 {
		printUsage(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "snapshot":
		os.Exit(snapshotCmd(os.Args[2:]))
	case "export":
		os.Exit(exportCmd(os.Args[2:]))
	case "exec":
		os.Exit(execCmd(os.Args[2:]))
	case "config":
		os.Exit(configCmd(os.Args[2:]))
	case "info":
		os.Exit(infoCmd(os.Args[2:]))
	case "publish":
		os.Exit(publishCmd(os.Args[2:]))
	case "upload-snapshot":
		os.Exit(uploadSnapshotCmd(os.Args[2:]))
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func validateSandboxIDArg(sandboxID string) error {
	if err := sandbox.ValidatePathID(sandboxID); err != nil {
		return fmt.Errorf("sandbox id must be one non-empty path component")
	}
	return nil
}

func validatePathIDArg(pathID string) error {
	return sandbox.ValidatePathID(pathID)
}

func resolveTargetPathID(sandboxID, pathID string) (string, error) {
	return sandbox.ResolvePathID(sandboxID, pathID)
}

func commandContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func printUsage(w *os.File) {
	fmt.Fprintf(w, `sandbox-ctl — sandbox runtime control

Usage:
  sandbox-ctl run       [--config host.yaml[:instance.yaml]] [--manifest-config <path>]
                        [--sandbox-id <sid>] [--path-id <leaf>] [--ch-binary <path>]
                        [--run-root <dir>] [--base-root <dir>]
                        [--from <sandbox-ref> [--replace-boot] | --restore <snapshot-ref>]
                        [--ref-location name=file:///absolute/path ...]
                        [--stdin] [--stdout=false] [--stderr=false]
                        [--stdin-from F] [--stdout-to TARGET] [--stderr-to TARGET]
                        [--tty] [--console off|default|file=PATH|journald=TAG[,FIELD=VALUE...]]
                        [--log-to default|journald=TAG[,FIELD=VALUE...]]
                        [--ping-fatal-threshold N] [--stats-interval <dur>]
                        [--ready-fd N]
  sandbox-ctl export    [--sandbox-id <sid>] [--path-id <leaf>]
                        (--output <out_dir> | --upload)
                        [--mode local|bundle] [--resume]
                        [--run-root <dir>] [--timeout <sec>]
  sandbox-ctl export    --from <flattened-erofs-ref> --config sandbox.yaml
                        [--sandbox-id <sid>] (--output <out_dir> | --upload)
                        [--mode local|bundle] [--manifest-config <path>]
                        [--ref-location name=file:///absolute/path ...]
                        [--timeout <sec>]
  sandbox-ctl snapshot  [--sandbox-id <sid>] [--path-id <leaf>]
                        (--output <out_dir> | --upload)
                        [--mode local|bundle] [--resume]
                        [--drop-caches] [--merge-ref=true|false]
                        [--run-root <dir>] [--timeout <sec>]
  sandbox-ctl exec      [--sandbox-id <sid>] [--path-id <leaf>] [--run-root <dir>]
                        [--proxy <http[s]://host[:port]>] [--proxy-header 'Name: value' ...]
                        [--cwd <dir>]
                        [--env KEY=VAL ...]
                        [--stdin] [--stdout=false] [--stderr=false]
                        [--stdin-from F] [--stdout-to TARGET] [--stderr-to TARGET]
                        [--tty] -- CMD [ARGS...]
  sandbox-ctl config    [--config a.yaml[:b.yaml...] | --template]
                        [--mode default|restore] [--check skip|strict] [-o <file>]
                        produce/merge/validate a sandbox.yaml on stdout
  sandbox-ctl info      [--json] [--manifest-config <p>]
                        [--ref-location name=file:///absolute/path ...] <artifact>
                        print sandbox.runtime.cfg or snapshot.cfg
  sandbox-ctl publish   [--manifest-config <p> | --to-ref-location name=file:///path]
                        [--ref-location name=file:///absolute/path ...] [--quiet] <artifact>
                        publish an E/S graph and print its rewritten root ref
  sandbox-ctl upload-snapshot
                        same options as publish; compatibility alias for Snapshot S

Output TARGET is a file path or journald=TAG[,FIELD=VALUE...]. Each journal
output owns its fields independently; values use one-time percent decoding.
No fields are inherited from other outputs or discovered from the environment.
run --log-to selects only component diagnostics and defaults to stderr; it does
not change guest stderr, TTY selection, or process descriptors. See docs/journald.md.

--manifest-config (or MANIFEST_CONFIG env) supplies the shared storage
configuration. It is required for any manifest:// resource and for local
file artifacts when crypto.local is auto or required. file://-only off-mode
configurations may omit it. The sensitive manifest.key may be supplied via
the MANIFEST_KEY env var instead of the config file (MANIFEST_KEY overrides
a manifest.key set in the file).

run starts one sandbox VM and blocks until the guest exits. Plain --config is
an explicit cold start. --from opens a Sandbox E and follows the same cold-start
path after applying allowed host/instance overrides. --restore opens a memory
Snapshot S, follows its sandbox_ref to E, and restores VMM/memory execution
state. --replace-boot makes --from use E only for non-boot defaults and takes
the complete boot definition from --config; it never applies to --restore.
--from and --restore are mutually exclusive. --ready-fd writes the one-shot startup wire
"control_ready\nready\n" to an inherited fd and closes that fd after
ready; run itself continues to own the VM and remains blocked. SandboxID is the
logical identity. PathID is the run-root/base-root directory leaf and defaults
to SandboxID; it must be one safe path component.

export creates a runnable Sandbox E. Live export freezes the guest and all
block backends but does not call Cloud Hypervisor's snapshot API or read RAM.
It reuses the storage and ref-location bindings owned by the running process.
Image-to-Sandbox-E assembly wraps one flattened EROFS image without starting a
VM and accepts storage/ref-location flags; assembly --resume is invalid. In
both modes --timeout=0 is unbounded.

snapshot always captures memory execution state. At one freeze point it emits
the current Sandbox E first and Snapshot S last; S points to E. Local output
defaults to content-addressed tarstreams, mode=bundle writes one Manifest
Bundle, and --upload writes the same logical graph to the Manifest store.
By default a successful live export/snapshot destroys the sandbox; --resume
keeps the original C0, writable diffs, and memory parent running. Recovery
reattaches the guest MUX before success is reported; an unrecoverable reattach
failure terminates the VMM instead of leaving a frozen sandbox behind.

exec runs an ad-hoc command inside a running sandbox as a sibling of
the user app (it does not replace it). The command + args follow '--'.
Stdio works exactly like run (--tty / --stdin / --stdout / --stderr
and their -from/-to variants). exec exits with the guest command's
exit code. Rejected while a snapshot is quiescing the sandbox. Without
--proxy it dials the local sandbox ctl.sock and requires --sandbox-id or
--path-id. When both are present, --path-id selects the socket; the ctl request
does not carry either identity.
With --proxy it sends HTTP CONNECT directly to that endpoint and adds
each repeatable --proxy-header without interpreting route/auth values.

See docs/sandbox.md for the full design.
`)
}
