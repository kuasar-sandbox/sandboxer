package artifact

import (
	"context"
	"errors"
	"io"
	"sort"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// memoryIndex fixes only the serving leaf and three-state coverage. It reads
// no payload and keeps no nested overlay/selection expression. A history of
// empty captures therefore has the same read cost as its original leaf.
type memoryIndex struct {
	size uint64
	runs []memoryIndexSpan
}

type memoryIndexSpan struct {
	off, end uint64
	kind     sparse.RunKind
	leaf     *memoryIndexLeaf
}

type memoryIndexLeaf struct {
	run sparse.Run
	// Manifest ChunkRuns must still be requested from their original leaf so
	// the UFFD chunk-window capability is preserved through the flattened view.
	chunkSource sparse.Source
}

type memoryIndexRun struct {
	span     memoryIndexSpan
	off, end uint64
}

func (r memoryIndexRun) Offset() uint64       { return r.off }
func (r memoryIndexRun) End() uint64          { return r.end }
func (r memoryIndexRun) Kind() sparse.RunKind { return r.span.kind }
func (r memoryIndexRun) ReadAt(ctx context.Context, b []byte, inner uint64) (int, error) {
	if inner > r.end-r.off || uint64(len(b)) > r.end-r.off-inner {
		return 0, errors.New("memory checkpoint: read outside indexed run")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.span.kind != sparse.Data {
		clear(b)
		return len(b), nil
	}
	return r.span.leaf.run.ReadAt(ctx, b, r.off+inner-r.span.leaf.run.Offset())
}

func (s *memoryIndex) Size() uint64 { return s.size }
func (s *memoryIndex) indexedRunAt(off, limit uint64) (memoryIndexRun, error) {
	if off >= s.size {
		return memoryIndexRun{}, io.EOF
	}
	if limit == 0 {
		return memoryIndexRun{}, errors.New("memory checkpoint: zero indexed run limit")
	}
	i := sort.Search(len(s.runs), func(i int) bool { return s.runs[i].end > off })
	if i == len(s.runs) || s.runs[i].off > off {
		return memoryIndexRun{}, errors.New("memory checkpoint: indexed coverage gap")
	}
	span := s.runs[i]
	return memoryIndexRun{span: span, off: off, end: off + min(limit, span.end-off)}, nil
}
func (s *memoryIndex) RunAt(off, limit uint64) (sparse.Run, error) {
	run, err := s.indexedRunAt(off, limit)
	if err != nil {
		return nil, err
	}
	if run.span.leaf != nil && run.span.leaf.chunkSource != nil {
		return run.span.leaf.chunkSource.RunAt(off, run.end-off)
	}
	return run, nil
}
func (s *memoryIndex) ReadAt(ctx context.Context, b []byte, off uint64) (int, error) {
	if off >= s.size {
		return 0, io.EOF
	}
	want := min(uint64(len(b)), s.size-off)
	var n uint64
	for n < want {
		run, err := s.RunAt(off+n, want-n)
		if err != nil {
			return int(n), err
		}
		count := run.End() - run.Offset()
		m, err := run.ReadAt(ctx, b[n:n+count], 0)
		n += uint64(m)
		if err != nil {
			return int(n), err
		}
		if uint64(m) != count {
			return int(n), io.ErrUnexpectedEOF
		}
	}
	if want < uint64(len(b)) {
		return int(want), io.EOF
	}
	return int(want), nil
}

// During composition every Data run carries its original leaf. Public reads
// instead return the leaf's own ChunkRun when it has that optional capability.
type memoryIndexBranch struct{ *memoryIndex }

func (s memoryIndexBranch) RunAt(off, limit uint64) (sparse.Run, error) {
	return s.indexedRunAt(off, limit)
}

func indexMemoryLeaf(ctx context.Context, source sparse.Source) (*memoryIndex, error) {
	return indexMemory(ctx, source, true)
}
func indexMemoryComposite(ctx context.Context, source sparse.Source) (*memoryIndex, error) {
	return indexMemory(ctx, source, false)
}
func indexMemory(ctx context.Context, source sparse.Source, leaf bool) (*memoryIndex, error) {
	if source == nil || source.Size() == 0 {
		return nil, errors.New("memory checkpoint: invalid indexed source")
	}
	indexed := &memoryIndex{size: source.Size()}
	for off := uint64(0); off < source.Size(); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := source.RunAt(off, source.Size()-off)
		if err != nil {
			return nil, err
		}
		if run == nil || run.Offset() != off || run.End() <= off || run.End() > source.Size() || run.Kind() > sparse.Data {
			return nil, errors.New("memory checkpoint: invalid indexed source run")
		}
		span := memoryIndexSpan{off: off, end: run.End(), kind: run.Kind()}
		if run.Kind() == sparse.Data {
			if previous, ok := run.(memoryIndexRun); ok {
				span.leaf = previous.span.leaf
			} else if leaf {
				span.leaf = &memoryIndexLeaf{run: run}
				if _, ok := run.(fetch.ChunkRun); ok {
					span.leaf.chunkSource = source
				}
			} else {
				return nil, errors.New("memory checkpoint: composite lost its serving leaf")
			}
		}
		last := len(indexed.runs) - 1
		if last >= 0 && indexed.runs[last].kind == span.kind && indexed.runs[last].leaf == span.leaf {
			indexed.runs[last].end = span.end
		} else {
			indexed.runs = append(indexed.runs, span)
		}
		off = span.end
	}
	return indexed, nil
}

func layerMemoryIndexes(sources []*memoryIndex) sparse.Source {
	layers := make([]sparse.Source, len(sources))
	for i, source := range sources {
		layers[i] = memoryIndexBranch{source}
	}
	return layerMemorySources(layers...)
}
