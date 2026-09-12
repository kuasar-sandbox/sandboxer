package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
	"golang.org/x/sys/unix"
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
	var value any
	if !*offline {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(*runRoot, leaf, "ctl.sock"))
		if dialErr == nil {
			defer conn.Close()
			end, _ := ctx.Deadline()
			if err := conn.SetDeadline(end); err != nil {
				return err
			}
			if err := ctl.WriteMessage(conn, ctl.Request{Type: ctl.TypeUsageRequest, UsageHistory: *history, UsageCursor: *cursor, UsageLimit: *limit}); err != nil {
				return err
			}
			response, err := ctl.ReadUsageResponse(conn)
			if err != nil {
				return err
			}
			if response.Type == ctl.TypeError {
				return errors.New(response.Msg)
			}
			if *saved && !*history {
				var view usage.View
				if err := json.Unmarshal(response.Usage, &view); err != nil {
					return err
				}
				view.Live = nil
				value = view
			} else {
				value = response.Usage
			}
		} else if !errors.Is(dialErr, syscall.ENOENT) && !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return dialErr
		}
	}
	if value == nil {
		path := *file
		if path == "" {
			path = filepath.Join(sandbox.DefaultBaseDir(*baseRoot, leaf), *sid+".usage")
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		// A readable CRC cannot certify that a running writer has accepted
		// its append; it may still roll F back. Query its ctl.sock S instead.
		if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			return fmt.Errorf("usage file has a live writer; query ctl.sock: %w", err)
		}
		stat, err := f.Stat()
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() {
			return errors.New("usage file is not regular")
		}
		recovered, err := usage.Recover(f, stat.Size(), *sid)
		if err != nil {
			return err
		}
		if *history {
			records, next, err := usage.ReadHistory(f, recovered.End, *cursor, *limit, *sid)
			if err != nil {
				return err
			}
			value = struct {
				Records []usage.Record `json:"records"`
				Next    int64          `json:"next_cursor,string"`
			}{records, next}
		} else {
			value = usage.View{Saved: recovered.Record, SavedEnd: recovered.End, UnknownTail: recovered.IncompleteTail}
		}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
