package diffstore

import (
	"context"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// activeSource preserves Zero classification instead of flattening every P=1
// page into Data. As with SnapshotView, the caller keeps the frontend quiescent;
// background writeback may continue and is reconciled below.
type activeSource struct{ reader *cowSnapshotReaderAt }

func (s *activeSource) Size() uint64 { return uint64(s.reader.cow.size) }
func (s *activeSource) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off > s.Size() {
		return 0, io.EOF
	}
	return s.reader.ReadAt(p, int64(off))
}
func (s *activeSource) RunAt(off, limit uint64) (sparse.Run, error) {
	if off >= s.Size() {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, fmt.Errorf("diffstore: zero export run limit")
	}
	c := s.reader.cow
	ctx, endOp, err := c.begin(nil)
	if err != nil {
		return nil, err
	}
	defer c.end(endOp)
	end := off + min(limit, s.Size()-off)
	first := off / cowBlockSize
	present := bitmapBlockDirty(s.reader.bitmap, int64(first))
	if present {
		end = min(end, (first+maxWritebackPages)*cowBlockSize)
	}
	runEnd := bitmapRunEnd(s.reader.bitmap, first, (end+cowBlockSize-1)/cowBlockSize)
	end = min(end, runEnd*cowBlockSize)
	kind := sparse.Hole
	if present {
		count := (end+cowBlockSize-1)/cowBlockSize - first
		var cached, payload [maxWritebackPages]bool
		// Capture accepted writes BEFORE the backend query. A writeback can turn
		// dirty into clean only after publishing its payload: either observation
		// therefore proves Data, even if the worker completes between the two.
		c.cache.mu.Lock()
		for i := uint64(0); i < count; i++ {
			p := c.cache.pages[cacheKey{c, int64(first + i)}]
			cached[i] = p != nil && (p.state == cacheDirty || p.state == cacheWriteback)
		}
		c.cache.mu.Unlock()
		if err := c.diff.payloadPresence(ctx, first, payload[:count]); err != nil {
			return nil, c.propagateBackendFailure(err)
		}
		data := cached[0] || payload[0]
		kind = sparse.Zero
		if data {
			kind = sparse.Data
		}
		for i := uint64(1); i < count; i++ {
			if (cached[i] || payload[i]) != data {
				end = (first + i) * cowBlockSize
				break
			}
		}
	}
	return activeSourceRun{s, off, end, kind}, nil
}

type activeSourceRun struct {
	s        *activeSource
	off, end uint64
	kind     sparse.RunKind
}

func (r activeSourceRun) Offset() uint64       { return r.off }
func (r activeSourceRun) End() uint64          { return r.end }
func (r activeSourceRun) Kind() sparse.RunKind { return r.kind }
func (r activeSourceRun) ReadAt(ctx context.Context, p []byte, inner uint64) (int, error) {
	if inner > r.end-r.off || uint64(len(p)) > r.end-r.off-inner {
		return 0, fmt.Errorf("diffstore: export run read out of bounds")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.kind != sparse.Data {
		clear(p)
		return len(p), nil
	}
	return r.s.ReadAt(ctx, p, r.off+inner)
}
