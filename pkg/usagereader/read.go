// Package usagereader reads the existing native usage state through its owner
// or, when no owner can be reached, under the saved file's shared lock. It does
// not sample, recover by writing, or change the native usage lifecycle.
package usagereader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
	"golang.org/x/sys/unix"
)

// Options contains paths already resolved by the owning CLI or conductor.
// SandboxID is the exact logical identity used by the native codec, never a
// StableID alias. File is also required for an online request's offline fallback.
type Options struct {
	SandboxID     string
	ControlSocket string
	File          string
	Offline       bool
	Saved         bool
	History       bool
	Cursor        int64
	Limit         int
}

// Read returns lossless native JSON: a usage.View, or records/next_cursor for a
// history request. Saved hides Live without changing saved/error/coverage data.
// A successful owner connection is authoritative even if its request fails;
// only an absent/refused socket can fall back to the file. A live writer's lock
// always prevents an offline reader from treating an unaccepted append as saved.
func Read(ctx context.Context, options Options) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.SandboxID == "" || options.File == "" || (!options.Offline && options.ControlSocket == "") || options.Cursor < 0 || options.Limit < 1 || options.Limit > 100 {
		return nil, errors.New("invalid usage query arguments")
	}
	if !options.Offline {
		response, connected, err := ctl.ReadUsage(ctx, options.ControlSocket, options.History, options.Cursor, options.Limit)
		if err == nil {
			if response.SandboxID != options.SandboxID {
				return nil, errors.New("usage: owner sandbox identity mismatch")
			}
			if options.History {
				var page *struct {
					Records []usage.Record `json:"records"`
					Next    *int64         `json:"next_cursor,string"`
				}
				if err := json.Unmarshal(response.Usage, &page); err != nil {
					return nil, err
				}
				if page == nil || page.Records == nil || page.Next == nil || *page.Next < options.Cursor ||
					len(page.Records) > options.Limit || (len(page.Records) == 0) != (*page.Next == options.Cursor) {
					return nil, errors.New("usage: invalid owner history")
				}
				for _, record := range page.Records {
					if record.Snapshot.SandboxID != options.SandboxID {
						return nil, errors.New("usage: sandbox identity mismatch")
					}
				}
				return response.Usage, nil
			}
			var view *usage.View
			if err := json.Unmarshal(response.Usage, &view); err != nil {
				return nil, err
			}
			if view == nil {
				return nil, errors.New("usage: invalid owner snapshot")
			}
			if (view.Live != nil && view.Live.SandboxID != options.SandboxID) || (view.Saved != nil && view.Saved.Snapshot.SandboxID != options.SandboxID) {
				return nil, errors.New("usage: sandbox identity mismatch")
			}
			if view.SavedEnd < 0 || (view.Saved != nil) != (view.SavedEnd > 0) {
				return nil, errors.New("usage: saved record and cursor disagree")
			}
			if !options.Saved {
				return response.Usage, nil
			}
			view.Live = nil
			return json.Marshal(view)
		}
		if connected || (!errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ECONNREFUSED)) {
			return nil, err
		}
	}
	return waitOffline(ctx, func() (json.RawMessage, error) { return readOffline(ctx, options) })
}

// File syscalls are not assumed interruptible. A canceled caller stops waiting,
// while the operation retains its slot and file lock until the syscall returns.
// This bounds stuck readers without changing Recover or native writer ownership.
var offlineSlots = make(chan struct{}, 8)

func waitOffline(ctx context.Context, read func() (json.RawMessage, error)) (json.RawMessage, error) {
	select {
	case offlineSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	type result struct {
		body json.RawMessage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-offlineSlots }()
		body, err := read()
		done <- result{body, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.body, result.err
	}
}

func readOffline(ctx context.Context, options Options) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A FIFO must not block before the regular-file check. O_NONBLOCK has no
	// effect on regular usage files and does not change their lock/recovery rules.
	f, err := os.OpenFile(options.File, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("usage file has a live writer; query ctl.sock: %w", err)
	}
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, errors.New("usage file is not regular")
	}
	reader := cancellableReader{ctx: ctx, file: f}
	recovered, err := usage.Recover(reader, stat.Size(), options.SandboxID)
	if err != nil {
		return nil, err
	}
	if options.History {
		return MarshalHistory(options.SandboxID, func(cursor int64, limit int) ([]usage.Record, int64, error) {
			return usage.ReadHistory(reader, recovered.End, cursor, limit, options.SandboxID)
		}, recovered.End, options.Cursor, options.Limit)
	}
	return json.Marshal(usage.View{Saved: recovered.Record, SavedEnd: recovered.End, UnknownTail: recovered.IncompleteTail})
}

type cancellableReader struct {
	ctx  context.Context
	file *os.File
}

func (r cancellableReader) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.ReadAt(p, offset)
}
