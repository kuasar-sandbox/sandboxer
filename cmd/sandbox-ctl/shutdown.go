package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
)

func shutdownCmd(args []string) int {
	ctx, cancel := commandContext()
	defer cancel()
	if err := runShutdown(ctx, args); err != nil {
		fmt.Fprintln(os.Stderr, "shutdown:", err)
		return 1
	}
	return 0
}

func runShutdown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("shutdown", flag.ContinueOnError)
	sid := fs.String("sandbox-id", "", "logical sandbox ID")
	runID := fs.String("run-id", "", "exact running generation")
	pathID := fs.String("path-id", "", "run directory leaf (defaults to sandbox ID)")
	runRoot := fs.String("run-root", "", "host control-socket root (overrides SANDBOX_RUN_ROOT env; default /run/sandbox)")
	timeout := fs.Duration("timeout", 0, "optional shorter operation budget (for example 30s); 0 uses the owner's bound")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *timeout < 0 {
		return errors.New("invalid shutdown arguments")
	}
	target := ctl.ShutdownTarget{SandboxID: *sid, RunID: *runID}
	if err := target.Validate(); err != nil {
		return err
	}
	leaf, err := resolveTargetPathID(*sid, *pathID)
	if err != nil {
		return err
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	rd := *runRoot
	if rd == "" {
		rd = os.Getenv("SANDBOX_RUN_ROOT")
	}
	if rd == "" {
		rd = "/run/sandbox"
	}
	return ctl.Shutdown(ctx, filepath.Join(rd, leaf, "ctl.sock"), target)
}
