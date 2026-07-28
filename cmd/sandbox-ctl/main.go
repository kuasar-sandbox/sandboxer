// sandbox-ctl is the host-side control tool for one sandbox lifecycle.
// Subcommands:
//
//	run       — start a sandbox (cold-start; or with --restore=<ref> from a snapshot)
//	snapshot  — pause + dump to <sid>.snapshot + <sha256>.overlay (or upload)
//	exec      — run an ad-hoc command inside a running sandbox
//	config    — produce / merge / validate a sandbox.yaml
//	info      — print a snapshot's embedded snapshot.cfg
//
// See docs/sandbox.md for the full design.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
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
	case "exec":
		os.Exit(execCmd(os.Args[2:]))
	case "config":
		os.Exit(configCmd(os.Args[2:]))
	case "info":
		os.Exit(infoCmd(os.Args[2:]))
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

func printUsage(w *os.File) {
	fmt.Fprintf(w, `sandbox-ctl — sandbox runtime control

Usage:
  sandbox-ctl run       --config sandbox.yaml [--manifest-config <path>]
                        [--sandbox-id <sid>] [--ch-binary <path>]
                        [--run-root <dir>] [--base-root <dir>]
                        [--restore <file_path|manifest://hex>]
                        [--restore-file-refs verify|trust]
                        [--stdin] [--stdout=false] [--stderr=false]
                        [--stdin-from F] [--stdout-to F] [--stderr-to F]
                        [--tty] [--console off|default|file=PATH]
                        [--ping-fatal-threshold N] [--stats-interval <dur>]
  sandbox-ctl snapshot  --sandbox-id <sid> (--output <out_dir> | --upload)
                        [--resume] [--run-root <dir>] [--timeout <sec>]
  sandbox-ctl exec      [--sandbox-id <sid>] [--run-root <dir>]
                        [--proxy <http[s]://host[:port]>] [--proxy-header 'Name: value' ...]
                        [--cwd <dir>]
                        [--env KEY=VAL ...]
                        [--stdin] [--stdout=false] [--stderr=false]
                        [--stdin-from F] [--stdout-to F] [--stderr-to F]
                        [--tty] -- CMD [ARGS...]
  sandbox-ctl config    [--config a.yaml[:b.yaml...] | --template]
                        [--mode default|restore] [--check skip|strict] [-o <file>]
                        produce/merge/validate a sandbox.yaml on stdout
  sandbox-ctl info      [--json] [--manifest-config <p>] <manifest://hex|snapshot-path>
                        print a snapshot's embedded snapshot.cfg
  sandbox-ctl upload-snapshot [--manifest-config <p>] [--quiet] <snapshot-path>
                        promote a LOCAL snapshot to a remote manifest:// snapshot (no boot)

--manifest-config (or MANIFEST_CONFIG env) is required for any
manifest:// resource (boot.root.base, --restore manifest://, --upload).
file://-only configurations may omit it. The sensitive manifest.key may
be supplied via the MANIFEST_KEY env var instead of the config file
(MANIFEST_KEY overrides a manifest.key set in the file).

run starts one sandbox VM and blocks until the guest exits. With
--restore, the sandbox is resumed from a snapshot bundle instead of
cold-starting (sandbox.yaml field semantics in restore mode are listed
in docs/sandbox.md §11.0).

snapshot pauses a running sandbox and writes a snapshot bundle either
to a local directory (--output) or to the manifest store (--upload).
The two are mutually exclusive. By default the sandbox is destroyed
after a successful snapshot; use --resume to keep it running.

exec runs an ad-hoc command inside a running sandbox as a sibling of
the user app (it does not replace it). The command + args follow '--'.
Stdio works exactly like run (--tty / --stdin / --stdout / --stderr
and their -from/-to variants). exec exits with the guest command's
exit code. Rejected while a snapshot is quiescing the sandbox. Without
--proxy it dials the local sandbox ctl.sock and requires --sandbox-id.
With --proxy it sends HTTP CONNECT directly to that endpoint and adds
each repeatable --proxy-header without interpreting route/auth values.

See docs/sandbox.md for the full design.
`)
}
