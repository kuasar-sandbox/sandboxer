package uffd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// StreamSnapshotSource implements SnapshotReader against a snapshot bundle's
// memory section, backed by any fetch.Stream — a local file (sparse-aware), a
// manifest (chunk-granular via cache-ctl), or an overlay of several (the
// incremental layered chain; see docs/sandbox.md §3.5). The memory section
// starts at bundle offset 0, sized RAMSize.
//
// Zero classification comes from Stream.RunAt: a Hole (merged across all
// layers — non-resident / declared hole everywhere) or an IsZero chunk needs
// no fetch and is served as a zero page; a Data run is fetched via
// Stream.ReadAt (from whichever layer holds it). The lazy-load ratio matches
// the underlying sparseness — no extra zero pages fetched, no hole pages
// over-served.
//
// Safe for concurrent use: the Stream is concurrent-safe and ctx is shared
// read-only.
type StreamSnapshotSource struct {
	stream  fetch.Stream
	ctx     context.Context
	ramSize uint64
}

// NewStreamSnapshotSource binds a stream to the snapshot bundle and captures a
// parent context that scopes all subsequent ReadAt calls (the SnapshotReader
// interface is sync; cancellation goes through this stored ctx).
//
// ramSize is the memory section size (= sandbox memory capacity); it must be ≤
// stream.Size() because the bundle = memory + ZIP and the source only serves
// the memory prefix.
func NewStreamSnapshotSource(ctx context.Context, stream fetch.Stream, ramSize uint64) (*StreamSnapshotSource, error) {
	if stream == nil {
		return nil, errors.New("uffd: nil stream")
	}
	if ramSize == 0 {
		return nil, errors.New("uffd: ramSize must be > 0")
	}
	if ramSize > stream.Size() {
		return nil, fmt.Errorf("uffd: ramSize %d exceeds bundle size %d", ramSize, stream.Size())
	}
	return &StreamSnapshotSource{
		stream:  stream,
		ctx:     ctx,
		ramSize: ramSize,
	}, nil
}

// ReadAt implements SnapshotReader.
//
// It classifies the run at memfdOffset via Stream.RunAt:
//   - memfdOffset ≥ ramSize → (0, true, io.EOF)
//   - a zero region (Hole or IsZero chunk) covering the whole first page →
//     (PageSize-aligned zero run, true, nil), no RPC issued
//   - otherwise → a data run, extended across contiguous data chunks up to
//     the next zero region or the buffer end, fetched via Stream.ReadAt and
//     returned (n, false, nil)
//
// A page straddling a zero→data boundary is served as data; Stream.ReadAt
// zero-fills the leading zero bytes, so real data is never served as zeros.
// The uploader's holes are page-aligned (memfd page granularity), so the
// straddle case only arises at end-of-image.
func (s *StreamSnapshotSource) ReadAt(buf []byte, memfdOffset uint64) (int, bool, error) {
	if memfdOffset >= s.ramSize {
		return 0, true, io.EOF
	}
	avail := s.ramSize - memfdOffset
	if uint64(len(buf)) > avail {
		buf = buf[:avail]
	}
	if len(buf) == 0 {
		return 0, true, nil
	}
	end := memfdOffset + uint64(len(buf))

	kind, runEnd, rerr := s.stream.RunAt(memfdOffset, end-memfdOffset)
	if rerr != nil { // io.EOF — guarded above (memfdOffset < ramSize ≤ Size)
		return 0, true, io.EOF
	}
	if kind != sparse.Data {
		// Zero region. Serve a zero page only when the whole first page (or
		// the rest of the image) is zero, so a page straddling zero→data is
		// never served as zeros.
		firstPageEnd := memfdOffset + PageSize
		if firstPageEnd > s.ramSize {
			firstPageEnd = s.ramSize
		}
		if runEnd >= firstPageEnd {
			zeroBytes := runEnd - memfdOffset
			if zeroBytes >= PageSize {
				zeroBytes -= zeroBytes % PageSize
			}
			return int(zeroBytes), true, nil
		}
		// Sub-page zero region not reaching the page end (mid-image straddle):
		// fall through and serve the page as data.
	} else {
		// Extend the data run across contiguous data chunks (RunAt caps each
		// Data run at one chunk) up to the next zero region or the buffer end,
		// so one fault fetches a whole data span concurrently.
		for runEnd < end {
			k2, e2, e2err := s.stream.RunAt(runEnd, end-runEnd)
			if e2err != nil || k2 != sparse.Data {
				break
			}
			runEnd = e2
		}
	}

	// Data run. Round down to a page boundary; a sub-page run only occurs at
	// end-of-image, where we serve the (≤ one page) remainder.
	runBytes := runEnd - memfdOffset
	if runBytes >= PageSize {
		runBytes -= runBytes % PageSize
	} else {
		runBytes = uint64(len(buf))
	}
	n, err := s.stream.ReadAt(s.ctx, buf[:runBytes], memfdOffset)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, false, fmt.Errorf("manifest snapshot ReadAt at %d: %w", memfdOffset, err)
	}
	// Zero-pad if the stream returned short (image ends inside this run).
	for i := n; i < int(runBytes); i++ {
		buf[i] = 0
	}
	return int(runBytes), false, nil
}
