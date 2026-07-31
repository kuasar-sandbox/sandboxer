package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"os"

	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
)

// uploadSnapshotCmd implements the offline local-ref upgrader. It publishes a
// local snapshot graph to manifest storage or one named file location, preserves
// existing portable refs, and prints the canonical portable root ref.
//
//	sandbox-ctl upload-snapshot [--manifest-config <file> | --to-ref-location name=file:///path] [--quiet] <snapshot-path>
func uploadSnapshotCmd(args []string) int {
	fs := flag.NewFlagSet("upload-snapshot", flag.ContinueOnError)
	manifestPath := fs.String("manifest-config", "", "manifest config YAML (overrides MANIFEST_CONFIG env); $MANIFEST_KEY supplies the customer key")
	toRefLocation := fs.String("to-ref-location", "", "publish local refs to name=file:///absolute/path")
	quiet := fs.Bool("quiet", false, "suppress progress logs on stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *toRefLocation != "" && *manifestPath != "" {
		fmt.Fprintln(os.Stderr, "upload-snapshot: --manifest-config and --to-ref-location are mutually exclusive")
		return 2
	}
	path := fs.Arg(0)
	if path == "" || fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: sandbox-ctl upload-snapshot [--manifest-config <file> | --to-ref-location name=file:///path] [--quiet] <snapshot-path>")
		return 2
	}

	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	if *quiet {
		logf = func(string, ...any) {}
	}
	var (
		ref string
		err error
	)
	if *toRefLocation != "" {
		locations := config.RefLocations{}
		if err := locations.Set(*toRefLocation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		for name, directory := range locations {
			ref, err = restore.PublishLocalToLocation(context.Background(), path, name, directory, logf)
		}
	} else {
		mcfg, loadErr := config.LoadManifestConfig(*manifestPath)
		if loadErr != nil {
			fmt.Fprintln(os.Stderr, loadErr)
			return 1
		}
		ref, err = restore.UploadLocal(context.Background(), path, mcfg, logf)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(ref) // manifest://<memKey>
	return 0
}
