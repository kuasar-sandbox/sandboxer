package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

// StreamSnapshotSource resolves a snapshot bundle's memory prefix through a
// fetch.Stream. Data Runs are returned directly, preserving capabilities such
// as fetch.ChunkRun. Final Hole and manifest Zero Runs are both exposed to UFFD
// as no-read Zero Runs.
type StreamSnapshotSource struct {
	stream  fetch.Stream
	ramSize uint64
}

// chunkWindowSource is the package-local optional capability used only by the
// manifest ChunkRun fault path. SnapshotReader stays unchanged; custom readers
// that cannot supply root layered visibility retain the safe forward Run.
type chunkWindowSource interface {
	resolveChunkWindow(anchor fetch.ChunkRun, maxBytes uint64) (fetch.ChunkRun, error)
}

// NewStreamSnapshotSource binds stream's memory prefix to a SnapshotReader.
// Read cancellation is supplied by the Handler to sparse.Run.ReadAt, so the
// source does not capture a separate context.
func NewStreamSnapshotSource(stream fetch.Stream, ramSize uint64) (*StreamSnapshotSource, error) {
	if stream == nil {
		return nil, errors.New("uffd: nil stream")
	}
	if ramSize == 0 {
		return nil, errors.New("uffd: ramSize must be > 0")
	}
	if ramSize > stream.Size() {
		return nil, fmt.Errorf("uffd: ramSize %d exceeds bundle size %d", ramSize, stream.Size())
	}
	return &StreamSnapshotSource{stream: stream, ramSize: ramSize}, nil
}

func (s *StreamSnapshotSource) resolveChunkWindow(anchor fetch.ChunkRun, maxBytes uint64) (fetch.ChunkRun, error) {
	window, err := fetch.ResolveChunkWindow(s.stream, anchor, maxBytes)
	if err != nil {
		return nil, err
	}
	// The SnapshotReader contract is bounded by RAM even when a caller binds a
	// raw carrier Stream instead of snapshotfile's memory section. Do not read
	// carrier bytes after RAM merely to expand a ChunkRun; the original anchor
	// is already clipped by RunAt and remains the safe fallback.
	if window.End() > s.ramSize {
		return anchor, nil
	}
	return window, nil
}

// RunAt implements SnapshotReader. It never reads payload data.
//
// A semantic no-read span merges adjacent final Hole and Zero Runs. If a
// zero/data boundary (in either direction) falls inside the first fault page,
// the whole visible part of that page is returned as an ordinary Data Run. Its
// ReadAt delegates to Stream.ReadAt so real data is never installed with
// UFFDIO_ZEROPAGE. This is the only case that combines Runs; normal Data Runs
// are returned unchanged and are never extended across chunk boundaries.
func (s *StreamSnapshotSource) RunAt(memfdOffset, limit uint64) (sparse.Run, error) {
	if memfdOffset >= s.ramSize {
		return nil, io.EOF
	}
	if limit == 0 {
		return nil, readerr.Mark(fmt.Errorf("uffd: snapshot RunAt: limit must be non-zero"), false)
	}

	bound := s.ramSize
	if limit < s.ramSize-memfdOffset {
		bound = memfdOffset + limit
	}
	firstPageEnd := bound
	if PageSize < bound-memfdOffset {
		firstPageEnd = memfdOffset + PageSize
	}

	run, err := s.stream.RunAt(memfdOffset, bound-memfdOffset)
	if err != nil {
		return nil, fmt.Errorf("uffd: snapshot RunAt at %d: %w", memfdOffset, err)
	}
	if err := validateResolvedSnapshotRun(run, memfdOffset, bound); err != nil {
		return nil, err
	}
	if run.Kind() == sparse.Data {
		if run.End() >= firstPageEnd {
			return run, nil
		}
		return streamPageRun{stream: s.stream, offset: memfdOffset, end: firstPageEnd}, nil
	}

	zeroEnd := run.End()
	for zeroEnd < bound {
		next, nextErr := s.stream.RunAt(zeroEnd, bound-zeroEnd)
		if nextErr != nil {
			return nil, fmt.Errorf("uffd: snapshot RunAt at %d: %w", zeroEnd, nextErr)
		}
		if err := validateResolvedSnapshotRun(next, zeroEnd, bound); err != nil {
			return nil, err
		}
		if next.Kind() == sparse.Data {
			break
		}
		zeroEnd = next.End()
	}
	if zeroEnd >= firstPageEnd {
		return snapshotZeroRun{offset: memfdOffset, end: zeroEnd}, nil
	}
	return streamPageRun{stream: s.stream, offset: memfdOffset, end: firstPageEnd}, nil
}

func validateResolvedSnapshotRun(run sparse.Run, offset, bound uint64) error {
	if run == nil {
		return readerr.Mark(fmt.Errorf("uffd: snapshot RunAt at %d returned nil", offset), false)
	}
	if run.Offset() != offset || run.End() <= offset || run.End() > bound {
		return readerr.Mark(fmt.Errorf("uffd: snapshot RunAt at %d returned invalid range [%d,%d), bound %d", offset, run.Offset(), run.End(), bound), false)
	}
	switch run.Kind() {
	case sparse.Hole, sparse.Zero, sparse.Data:
		return nil
	default:
		return readerr.Mark(fmt.Errorf("uffd: snapshot RunAt at %d returned invalid kind %d", offset, run.Kind()), false)
	}
}

// streamPageRun is the page-safety fallback for a semantic zero/data boundary
// inside the first fault page. It deliberately does not implement
// fetch.ChunkRun because serving it can require more than one physical leaf.
type streamPageRun struct {
	stream fetch.Stream
	offset uint64
	end    uint64
}

func (r streamPageRun) Offset() uint64     { return r.offset }
func (r streamPageRun) End() uint64        { return r.end }
func (streamPageRun) Kind() sparse.RunKind { return sparse.Data }

func (r streamPageRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	if err := validateSnapshotRunRead(r.offset, r.end, innerOffset, len(buf)); err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, nil
	}
	n, err := r.stream.ReadAt(ctx, buf, r.offset+innerOffset)
	if n == len(buf) && !readerr.IsPermanent(err) && !readretry.IsTerminal(err) && (err == nil || err == io.EOF) {
		return n, nil
	}
	if err != nil {
		return n, fmt.Errorf("uffd: snapshot page read at %d: %w", r.offset+innerOffset, err)
	}
	return n, readerr.Mark(fmt.Errorf("uffd: snapshot page short read at %d: %d of %d bytes: %w", r.offset+innerOffset, n, len(buf), io.ErrUnexpectedEOF), false)
}
