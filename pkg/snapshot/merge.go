package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// tarLayer is a parent local artifact (tarstream envelope) opened as a
// closable ReadSeeker over its logical view. Used as the merge BASE: the
// parent local bundle's memory section, or the parent local overlay. Reads
// past the layer size (e.g. into a .snapshot's ZIP trailer) never happen —
// mergedReadSeeker bounds every read by its size.
type tarLayer struct {
	stream fetch.Stream
	io.ReadSeeker
}

// MergeBaseOpener returns one selected local layer as a fetch.Stream. The
// caller owns the stream. It lets Take merge tarstream and Manifest Bundle
// parents through the same sparse interface.
type MergeBaseOpener func(ctx context.Context, raw string) (fetch.Stream, error)

func (l *tarLayer) Close() error { return l.stream.Close() }

// openMergeBase opens a parent local artifact ref and returns its logical view
// plus the holes over [0,size) — from the tar envelope's map, clipped to the
// layer size (a snapshot's memory section = MemfdSize; an overlay's = the
// diff size). The ref is an already-resolved file ref with an explicit expected
// identity, so logical basenames remain valid without a basename guess. size
// must not exceed the entry's logical size.
func openMergeBase(raw string, size int64, codec tarstream.Codec, required bool) (*tarLayer, []sparse.Extent, error) {
	return openMergeBaseWithOpener(context.Background(), raw, size, codec, required, nil)
}

func openMergeBaseWithOpener(ctx context.Context, raw string, size int64, codec tarstream.Codec, required bool, opener MergeBaseOpener) (*tarLayer, []sparse.Extent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if size < 0 {
		return nil, nil, fmt.Errorf("merge base: negative logical size")
	}
	if required && codec == nil {
		return nil, nil, fmt.Errorf("merge base: required policy has no codec")
	}
	if opener != nil {
		stream, err := opener(ctx, raw)
		if err != nil {
			return nil, nil, &mergeArtifactError{err: err}
		}
		if stream == nil {
			return nil, nil, &mergeArtifactError{err: errors.New("opener returned nil stream")}
		}
		return prepareMergeBase(ctx, stream, size)
	}
	ref, err := manifest.ParseRef(raw)
	if err != nil || ref.Scheme != manifest.RefSchemeFile || ref.Location != "" || ref.Digest == "" || ref.DigestScheme == "manifest" {
		return nil, nil, fmt.Errorf("merge base: resolved tarstream file identity required")
	}
	var options []tarstream.ReadOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, required))
	}
	options = append(options, tarstream.WithExpectedDigest(ref.DigestScheme, ref.Digest))
	stream, err := fetch.OpenTarStream(ref.Path, options...)
	if err != nil {
		return nil, nil, &mergeArtifactError{err: err}
	}
	return prepareMergeBase(ctx, stream, size)
}

func prepareMergeBase(ctx context.Context, stream fetch.Stream, size int64) (*tarLayer, []sparse.Extent, error) {
	fail := func(err error) (*tarLayer, []sparse.Extent, error) {
		_ = stream.Close()
		return nil, nil, err
	}
	if stream.Size() < uint64(size) {
		return fail(fmt.Errorf("merge base: entry size %d < expected layer size %d", stream.Size(), size))
	}
	holes, err := mergeStreamHoles(stream, uint64(size))
	if err != nil {
		return fail(fmt.Errorf("merge base: hole map: %w", err))
	}
	reader := fetch.NewReaderAt(ctx, stream)
	return &tarLayer{stream: stream, ReadSeeker: io.NewSectionReader(reader, 0, size)}, holes, nil
}

type mergeArtifactError struct{ err error }

func (e *mergeArtifactError) Error() string { return "merge base: local artifact validation failed" }
func (e *mergeArtifactError) Unwrap() error { return e.err }

// ValidateMergeBase performs the same structural, expected-identity, and logical
// size checks Take will apply to a local memory or disk merge base, without
// retaining the artifact. Callers use it before guest quiesce so predictable
// local-artifact failures cannot leave a guest frozen.
func ValidateMergeBase(path string, size int64, codec tarstream.Codec, required bool) error {
	return ValidateMergeBaseWithOpener(context.Background(), path, size, codec, required, nil)
}

func ValidateMergeBaseWithOpener(ctx context.Context, path string, size int64, codec tarstream.Codec, required bool, opener MergeBaseOpener) error {
	base, _, err := openMergeBaseWithOpener(ctx, path, size, codec, required, opener)
	if err != nil {
		return err
	}
	return base.Close()
}

func mergeStreamHoles(stream fetch.Stream, size uint64) ([]sparse.Extent, error) {
	var holes []sparse.Extent
	for offset := uint64(0); offset < size; {
		run, err := stream.RunAt(offset, size-offset)
		if err != nil {
			return nil, err
		}
		if run == nil || run.Offset() != offset || run.End() <= offset || run.End() > size {
			return nil, fmt.Errorf("invalid run at offset %d", offset)
		}
		kind, end := run.Kind(), run.End()
		if kind == sparse.Hole {
			holes = append(holes, sparse.Extent{Offset: offset, Size: end - offset})
		}
		offset = end
	}
	return holes, nil
}

// mergeSparse flattens two adjacent sparse layers — top (this run's resident
// delta) over base (the local parent snapshot this run was restored from) —
// into a single layer, for the local "replace the next-newest layer" export
// (docs/sandbox.md §11.1 and §12.2). It is the SAVE-side dual of
// fetch.Layered: at any
// offset top's byte wins where top is resident; where top is a hole, base's
// byte shows through; where BOTH are holes the position stays a (merged) hole
// that falls through to the parent's OWN from_refs / base_from_refs (the
// grandparent chain) at restore.
//
// Semantics MUST match fetch.Layered so that restoring the merged single layer
// is byte-identical to restoring the original [top, base] two-layer chain
// (asserted by merge_test.go). Local sparse files only ever classify as Data or
// Hole (no IsZero — that is a manifest-chunk concept), so a hole-intersection is
// the complete merge rule.
//
// Returns a ReadSeeker over [0,size) plus the merged hole map. ArtifactSink
// consumes only non-hole data segments, so the merge remains sparse.
func mergeSparse(top io.ReadSeeker, topHoles []sparse.Extent, base io.ReadSeeker, baseHoles []sparse.Extent, size int64) (io.ReadSeeker, []sparse.Extent) {
	m := &mergedReadSeeker{top: top, topHoles: topHoles, base: base, baseHoles: baseHoles, size: size}
	return m, holeIntersection(topHoles, baseHoles, size)
}

type layerKind uint8

const (
	fromTop layerKind = iota
	fromBase
	fromHole
)

// mergedReadSeeker presents (top over base) as a single io.ReadSeeker. pos
// advances monotonically when driven by the sink's artifact packer (Seek to a
// data segment, then sequential reads).
type mergedReadSeeker struct {
	top, base           io.ReadSeeker
	topHoles, baseHoles []sparse.Extent
	size, pos           int64
}

// ownerAt returns which layer serves pos and the end of its contiguous run
// (≤ size): top where top is resident; base where top is a hole but base is
// resident; hole where both are holes.
func (m *mergedReadSeeker) ownerAt(pos int64) (layerKind, int64) {
	topHole, topEnd := holeRun(pos, m.topHoles, m.size)
	if !topHole {
		return fromTop, topEnd
	}
	baseHole, baseEnd := holeRun(pos, m.baseHoles, m.size)
	end := min(topEnd, baseEnd) // run ends as soon as EITHER layer's state changes
	if !baseHole {
		return fromBase, end
	}
	return fromHole, end
}

func (m *mergedReadSeeker) Read(p []byte) (int, error) {
	if m.pos >= m.size {
		return 0, io.EOF
	}
	owner, end := m.ownerAt(m.pos)
	n := int64(len(p))
	if rem := end - m.pos; n > rem {
		n = rem
	}
	if n <= 0 {
		return 0, io.EOF
	}
	switch owner {
	case fromTop:
		return m.readFrom(m.top, p[:n])
	case fromBase:
		return m.readFrom(m.base, p[:n])
	default: // fromHole — defensive zero-fill (the sink skips merged holes)
		clear(p[:n])
		m.pos += n
		return int(n), nil
	}
}

func (m *mergedReadSeeker) readFrom(layer io.ReadSeeker, p []byte) (int, error) {
	if _, err := layer.Seek(m.pos, io.SeekStart); err != nil {
		return 0, err
	}
	rd, err := io.ReadFull(layer, p)
	m.pos += int64(rd)
	if err == io.ErrUnexpectedEOF {
		err = nil // short read at the layer's end; pos advanced by what we got
	}
	return rd, err
}

func (m *mergedReadSeeker) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = m.pos + off
	case io.SeekEnd:
		abs = m.size + off
	default:
		return 0, fmt.Errorf("merge: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("merge: negative position %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// holeRun reports whether pos lies in a hole and the end of the current run
// (hole-end if in a hole, else the start of the next hole, or size). holes must
// be sorted, non-overlapping, within [0,size). Use binary search because a
// BlockCOW snapshot can legitimately contain millions of alternating 4 KiB
// hole/data runs; rescanning from the first extent for every RunAt call would
// make artifact creation quadratic while the VM is paused.
func holeRun(pos int64, holes []sparse.Extent, size int64) (bool, int64) {
	if pos < 0 {
		return false, 0
	}
	if pos >= size {
		return false, size
	}
	position := uint64(pos)
	i := sort.Search(len(holes), func(i int) bool {
		h := holes[i]
		return h.Offset > position || h.Size > position-h.Offset
	})
	if i == len(holes) {
		return false, size
	}
	h := holes[i]
	if pos < int64(h.Offset) {
		return false, int64(h.Offset)
	}
	return true, int64(h.Offset + h.Size)
}

// holeIntersection returns the ranges that are holes in BOTH inputs over
// [0,size) — positions with no data in either layer, which become merged holes
// that fall through to the grandparent chain at restore.
func holeIntersection(a, b []sparse.Extent, size int64) []sparse.Extent {
	var out []sparse.Extent
	for pos := int64(0); pos < size; {
		aHole, aEnd := holeRun(pos, a, size)
		bHole, bEnd := holeRun(pos, b, size)
		end := min(aEnd, bEnd)
		if aHole && bHole {
			if n := len(out); n > 0 && int64(out[n-1].Offset+out[n-1].Size) == pos {
				out[n-1].Size += uint64(end - pos) // coalesce contiguous run
			} else {
				out = append(out, sparse.Extent{Offset: uint64(pos), Size: uint64(end - pos)})
			}
		}
		pos = end
	}
	return out
}
