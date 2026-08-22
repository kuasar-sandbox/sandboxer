package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
)

// infoCmd implements `sandbox-ctl info` — print a snapshot's embedded
// snapshot.cfg (the post-quiesce platform contract: capacity, runtime_ref,
// base_ref, overlay.base; docs/sandbox.md §3.4). Mirrors `flatten-ctl info`.
//
//	sandbox-ctl info [--json] [--manifest-config <file>] <manifest://hex | snapshot-path>
//
// Default output is the raw snapshot.cfg YAML; --json re-emits the parsed struct.
func infoCmd(args []string) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	manifestPath := fs.String("manifest-config", "", "storage config YAML (overrides MANIFEST_CONFIG env); required for manifest:// or crypto.local=auto|required")
	refLocations := config.RefLocations{}
	fs.Var(refLocations, "ref-location", "trusted ref location name=file:///absolute/path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	input := fs.Arg(0)
	if input == "" {
		fmt.Fprintln(os.Stderr, "usage: sandbox-ctl info [--json] [--manifest-config <file>] <manifest://hex|snapshot-path>")
		return 2
	}

	ctx := context.Background()
	manifestCfg, err := config.LoadManifestConfig(*manifestPath)
	if err != nil {
		if !errors.Is(err, manifest.ErrConfigNotProvided) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		manifestCfg = nil
	}
	reader, err := restore.NewSnapshotCfgReader(manifestCfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer reader.Close()
	document, err := reader.Read(ctx, input, restore.SnapshotCfgReadOptions{RefLocations: refLocations})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(document.Config); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if _, err := os.Stdout.Write(document.Raw); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
