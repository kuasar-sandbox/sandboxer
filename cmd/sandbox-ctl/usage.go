package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/usagereader"
)

func usageCmd(args []string) int {
	if err := runUsage(args, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runUsage(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	sid := fs.String("sandbox-id", "", "logical sandbox ID")
	pathID := fs.String("path-id", "", "existing run/base directory leaf (defaults to sandbox ID)")
	runRoot := fs.String("run-root", "/run/sandbox", "host control-socket root")
	baseRoot := fs.String("base-root", sandbox.DefaultBaseRoot, "persistent sandbox root")
	file := fs.String("file", "", "explicit offline .usage path")
	offline := fs.Bool("offline", false, "read saved data without contacting a running sandbox")
	saved := fs.Bool("saved", false, "return only the saved snapshot")
	history := fs.Bool("history", false, "page through complete saved records")
	cursor := fs.Int64("cursor", 0, "history byte cursor")
	limit := fs.Int("limit", 10, "history records per page (1..100)")
	timeout := fs.Duration("timeout", 5*time.Second, "online query budget")
	jsonOutput := fs.Bool("json", true, "output lossless JSON (the only usage output format)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 100 || *cursor < 0 || *timeout <= 0 || !*jsonOutput {
		return errors.New("invalid usage query arguments")
	}
	if *file != "" {
		*offline = true
		if *sid == "" {
			*sid = strings.TrimSuffix(filepath.Base(*file), ".usage")
		}
	}
	if *sid == "" {
		return errors.New("usage requires --sandbox-id or --file")
	}
	if err := sandbox.ValidatePathID(*sid); err != nil {
		return err
	}
	leaf, err := sandbox.ResolvePathID(*sid, *pathID)
	if err != nil {
		return err
	}
	path := *file
	if path == "" {
		path = filepath.Join(sandbox.DefaultBaseDir(*baseRoot, leaf), *sid+".usage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	value, err := usagereader.Read(ctx, usagereader.Options{
		SandboxID: *sid, ControlSocket: filepath.Join(*runRoot, leaf, "ctl.sock"), File: path,
		Offline: *offline, Saved: *saved, History: *history, Cursor: *cursor, Limit: *limit,
	})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
