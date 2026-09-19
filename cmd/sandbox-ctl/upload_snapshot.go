package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

type repeatedFlag []string

func (f *repeatedFlag) String() string { return fmt.Sprint([]string(*f)) }
func (f *repeatedFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

// publishCmd publishes either a strict Sandbox E or Snapshot S. The historical
// upload-snapshot command calls the same implementation as a thin CLI alias.
func publishCmd(args []string) int { return publishArtifactCmd("publish", args) }

func uploadSnapshotCmd(args []string) int { return publishArtifactCmd("upload-snapshot", args) }

func publishArtifactCmd(command string, args []string) int {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	manifestPath := fs.String("manifest-config", "", "storage/Bundle config YAML (overrides MANIFEST_CONFIG env); $MANIFEST_KEY supplies the customer key")
	toRefLocation := fs.String("to-ref-location", "", "publish to name=file:///absolute/path instead of the manifest store")
	refLocations := config.RefLocations{}
	fs.Var(refLocations, "ref-location", "trusted input ref location name=file:///absolute/path (repeatable)")
	var replaceRefs, reduceRefs repeatedFlag
	fs.Var(&replaceRefs, "replace-ref", "replace one reference OLD=NEW (repeatable for disjoint references)")
	fs.Var(&reduceRefs, "reduce-ref", "reduce a complete reference chain A=X, A, or any (disjoint from replacement rules)")
	skipVerifyRef := fs.Bool("skip-verify-ref", false, "skip replacement equivalence proof (identity and I/O checks remain enabled)")
	quiet := fs.Bool("quiet", false, "suppress progress logs on stderr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	input := fs.Arg(0)
	if input == "" || fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: sandbox-ctl %s [--manifest-config <file>] [--to-ref-location name=file:///path] [--ref-location name=file:///path ...] [--replace-ref OLD=NEW ...] [--reduce-ref A[=X] ...|--reduce-ref=any] [--skip-verify-ref] [--quiet] <artifact>\n", command)
		return 2
	}
	rewrite, err := artifact.ParseRewriteOptions(replaceRefs, reduceRefs, *skipVerifyRef)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	logf := func(format string, values ...any) { fmt.Fprintf(os.Stderr, format+"\n", values...) }
	if *quiet {
		logf = func(string, ...any) {}
	}
	var targetName, targetDirectory string
	if *toRefLocation != "" {
		targets := config.RefLocations{}
		if err := targets.Set(*toRefLocation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		for targetName, targetDirectory = range targets {
			if inputDirectory, exists := refLocations[targetName]; exists && inputDirectory != targetDirectory {
				fmt.Fprintf(os.Stderr, "publish target ref location %q conflicts with input ref location: %q != %q\n", targetName, targetDirectory, inputDirectory)
				return 2
			}
			// Preserve the existing behavior where a target location also resolves
			// source refs bearing that name when no separate input mapping exists.
			refLocations[targetName] = targetDirectory
		}
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
	var publisher *artifact.Publisher
	if targetName == "" {
		publisher, err = artifact.NewManifestPublisher(storage, manifestCfg, refLocations, logf)
	} else {
		publisher, err = artifact.NewLocationPublisher(storage, targetName, targetDirectory, refLocations, logf)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, errors.Join(err, storage.Close()))
		return 1
	}
	ctx, stopSignals := commandContext()
	defer stopSignals()
	result, publishErr := publisher.PublishWithOptions(ctx, input, rewrite)
	closeErr := errors.Join(publisher.Close(), storage.Close())
	if err := errors.Join(publishErr, closeErr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, err := fmt.Fprintln(os.Stdout, result.Ref); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
