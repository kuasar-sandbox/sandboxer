package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// publishCmd publishes either a strict Sandbox E or Snapshot S. The historical
// upload-snapshot command calls the same implementation as a thin CLI alias.
func publishCmd(args []string) int { return publishArtifactCmd("publish", args) }

func uploadSnapshotCmd(args []string) int { return publishArtifactCmd("upload-snapshot", args) }

func publishArtifactCmd(command string, args []string) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	manifestPath := fs.String("manifest-config", "", "storage config YAML (overrides MANIFEST_CONFIG env); $MANIFEST_KEY supplies the customer key")
	toRefLocation := fs.String("to-ref-location", "", "publish to name=file:///absolute/path instead of the manifest store")
	refLocations := config.RefLocations{}
	fs.Var(refLocations, "ref-location", "trusted input ref location name=file:///absolute/path (repeatable)")
	quiet := fs.Bool("quiet", false, "suppress progress logs on stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	input := fs.Arg(0)
	if input == "" || fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: sandbox-ctl %s [--manifest-config <file> | --to-ref-location name=file:///path] [--ref-location name=file:///path ...] [--quiet] <artifact>\n", command)
		return 2
	}
	logf := func(format string, values ...any) { fmt.Fprintf(os.Stderr, format+"\n", values...) }
	if *quiet {
		logf = func(string, ...any) {}
	}
	manifestCfg, loadErr := config.LoadManifestConfig(*manifestPath)
	if loadErr != nil {
		if *toRefLocation == "" || !errors.Is(loadErr, manifest.ErrConfigNotProvided) {
			fmt.Fprintln(os.Stderr, loadErr)
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
	var publisher *artifact.Publisher
	if *toRefLocation == "" {
		publisher, err = artifact.NewManifestPublisher(storage, manifestCfg, refLocations, logf)
	} else {
		targets := config.RefLocations{}
		if err := targets.Set(*toRefLocation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		for name, directory := range targets {
			refLocations[name] = directory
			publisher, err = artifact.NewLocationPublisher(storage, name, directory, refLocations, logf)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	result, publishErr := publisher.Publish(context.Background(), input)
	closeErr := publisher.Close()
	if err := errors.Join(publishErr, closeErr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(result.Ref)
	return 0
}
