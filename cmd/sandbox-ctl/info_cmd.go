package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// infoCmd implements `sandbox-ctl info` for both strict logical roots:
// sandbox.runtime.cfg for Sandbox E and snapshot.cfg for Snapshot S.
//
//	sandbox-ctl info [--json] [--manifest-config <file>] <artifact-ref-or-path>
//
// Default output is the canonical raw YAML; --json emits the parsed schema.
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
	if input == "" || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandbox-ctl info [--json] [--manifest-config <file>] [--ref-location name=file:///path ...] <artifact-ref-or-path>")
		return 2
	}

	ctx, stopSignals := commandContext()
	defer stopSignals()
	manifestCfg, err := config.LoadManifestConfig(*manifestPath)
	if err != nil {
		if !errors.Is(err, manifest.ErrConfigNotProvided) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		manifestCfg = nil
	}
	storage, err := artifact.NewProcessStorage(manifestCfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer storage.Close()
	document, err := storage.Inspect(ctx, input, refLocations)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *asJSON {
		var value any = document.Sandbox
		if document.Snapshot != nil {
			value = document.Snapshot
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(value); err != nil {
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
