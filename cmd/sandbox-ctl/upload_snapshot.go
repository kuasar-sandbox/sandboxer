package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"os"

	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
)

// uploadSnapshotCmd implements `sandbox-ctl upload-snapshot` — promote a LOCAL
// file snapshot to a REMOTE manifest:// snapshot WITHOUT booting a sandbox
// (docs/sandbox.md §3.5). It ingests this snapshot's OWN overlay + memory bundle
// and carries the (already-remote) lower chain by reference, after verifying
// each lower layer is present and sealed under the current MANIFEST_KEY (manifest
// blob only — no chunk download). Prints the uploaded snapshot's manifest:// key.
//
//	sandbox-ctl upload-snapshot [--manifest-config <file>] [--quiet] <snapshot-path>
//
// Needs --manifest-config (or MANIFEST_CONFIG) + $MANIFEST_KEY; no /dev/kvm, no
// running sandbox. A lower local file:// layer is rejected — re-export it
// locally first to flatten it into the top layer.
func uploadSnapshotCmd(args []string) int {
	fs := flag.NewFlagSet("upload-snapshot", flag.ContinueOnError)
	manifestPath := fs.String("manifest-config", "", "manifest config YAML (overrides MANIFEST_CONFIG env); $MANIFEST_KEY supplies the customer key")
	quiet := fs.Bool("quiet", false, "suppress progress logs on stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := fs.Arg(0)
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: sandbox-ctl upload-snapshot [--manifest-config <file>] [--quiet] <snapshot-path>")
		return 2
	}

	mcfg, err := config.LoadManifestConfig(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	if *quiet {
		logf = func(string, ...any) {}
	}
	ref, err := restore.UploadLocal(context.Background(), path, mcfg, logf)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(ref) // manifest://<memKey>
	return 0
}
