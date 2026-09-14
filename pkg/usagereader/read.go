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
			if options.History {
				var page *struct {
					Records []usage.Record `json:"records"`
				}
				if err := json.Unmarshal(response.Usage, &page); err != nil {
					return nil, err
				}
				if page == nil {
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(options.File)
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
		return MarshalHistory(func(cursor int64, limit int) ([]usage.Record, int64, error) {
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
