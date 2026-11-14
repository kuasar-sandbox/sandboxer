package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

func diskCmd(args []string) int {
	if err := runDisk(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runDisk(args []string) error {
	if len(args) == 0 || (args[0] != "bind" && args[0] != "list") {
		return errors.New("usage: sandbox-ctl disk bind|list --path-id ID")
	}
	fs := flag.NewFlagSet("disk "+args[0], flag.ContinueOnError)
	id := fs.String("path-id", "", "running sandbox path identity")
	root := fs.String("run-root", os.Getenv("SANDBOX_RUN_ROOT"), "control socket root")
	expected := fs.String("expected-run", "", "require this running owner")
	var name, source string
	loc := config.RefLocations{}
	if args[0] == "bind" {
		fs.StringVar(&name, "disk", "", "target slot name")
		fs.StringVar(&source, "disk-spec", "", "complete disk definition file (- for stdin)")
		fs.Var(loc, "ref-location", "trusted name=file:///absolute/path (repeatable)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional argument")
	}
	if err := validatePathIDArg(*id); err != nil {
		return err
	}
	if *root == "" {
		*root = "/run/sandbox"
	}
	path := filepath.Join(*root, *id, "ctl.sock")
	ctx, cancel := commandContext()
	defer cancel()
	if args[0] == "list" {
		report, err := ctl.ListDataDisks(ctx, path, *expected)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	if name == "" || source == "" {
		return errors.New("disk bind requires --disk and --disk-spec")
	}
	file := os.Stdin
	if source != "-" {
		f, err := os.Open(source)
		if err != nil {
			return err
		}
		defer f.Close()
		file = f
	}
	b, err := io.ReadAll(io.LimitReader(file, config.MaxPortableConfigBytes+1))
	if err != nil {
		return err
	}
	if len(b) > config.MaxPortableConfigBytes {
		return errors.New("disk definition exceeds size limit")
	}
	var disk config.DiskV1
	if err := json.Unmarshal(b, &disk); err != nil {
		return err
	}
	return ctl.BindDisk(ctx, path, ctl.BindDiskRequest{SlotName: name, Disk: disk, RefLocations: loc, ExpectedRun: *expected})
}
