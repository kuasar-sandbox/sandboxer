package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"io"
	"os"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
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
	manifestPath := fs.String("manifest-config", "", "manifest config YAML (overrides MANIFEST_CONFIG env); required for manifest:// inputs")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	input := fs.Arg(0)
	if input == "" {
		fmt.Fprintln(os.Stderr, "usage: sandbox-ctl info [--json] [--manifest-config <file>] <manifest://hex|snapshot-path>")
		return 2
	}

	ctx := context.Background()
	var (
		stream    fetch.Stream
		totalSize int64
	)
	if strings.HasPrefix(input, "manifest://") {
		key := strings.TrimPrefix(input, "manifest://")
		mcfg, err := config.LoadManifestConfig(*manifestPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fetcher, err := mcfg.NewFetcher(mcfg.FetchKeyFunc())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer fetcher.Close()
		fc, sz, err := sandbox.OpenManifestStream(ctx, key, fetcher)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer fc.Close()
		stream, totalSize = fc, sz
	} else {
		fsr, err := fetch.OpenTarStream(input)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer fsr.Close()
		stream, totalSize = fsr, int64(fsr.Size())
	}

	body, err := readSnapshotCfg(fetch.NewReaderAt(ctx, stream, totalSize), totalSize)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if *asJSON {
		snap, err := restore.ParseSnapshotCfg(body)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snap); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	os.Stdout.Write(body)
	return 0
}

// readSnapshotCfg extracts the trailing-ZIP "snapshot.cfg" entry from a snapshot
// image (the same bundle restore reads: config.json / state.json / snapshot.cfg).
func readSnapshotCfg(ra io.ReaderAt, size int64) ([]byte, error) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return nil, fmt.Errorf("read snapshot zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name == "snapshot.cfg" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("input has no snapshot.cfg entry (not a snapshot image?)")
}
