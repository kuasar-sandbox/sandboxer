package uffd

import (
	"context"
	"fmt"
	"math"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// SnapshotReader resolves the snapshot contents covering memfdOffset. RunAt is
// metadata-only; payload bytes are read later through the returned sparse.Run
// with offsets relative to Run.Offset(). Implementations must be safe for
// concurrent RunAt calls, and returned Runs remain valid for the lifetime of
// the reader that produced them.
type SnapshotReader interface {
	RunAt(memfdOffset, limit uint64) (sparse.Run, error)
}

// ZeroSource implements SnapshotReader for cold-start. It does not choose a
// speculative window: every returned Zero Run is bounded exactly by limit.
type ZeroSource struct{}

func (ZeroSource) RunAt(memfdOffset, limit uint64) (sparse.Run, error) {
	if limit == 0 {
		return nil, fmt.Errorf("uffd: zero source: limit must be non-zero")
	}
	if limit > math.MaxUint64-memfdOffset {
		return nil, fmt.Errorf("uffd: zero source: range overflows uint64")
	}
	return snapshotZeroRun{offset: memfdOffset, end: memfdOffset + limit}, nil
}

type snapshotZeroRun struct {
	offset uint64
	end    uint64
}

func (r snapshotZeroRun) Offset() uint64     { return r.offset }
func (r snapshotZeroRun) End() uint64        { return r.end }
func (snapshotZeroRun) Kind() sparse.RunKind { return sparse.Zero }
func (r snapshotZeroRun) ReadAt(ctx context.Context, buf []byte, innerOffset uint64) (int, error) {
	if err := validateSnapshotRunRead(r.offset, r.end, innerOffset, len(buf)); err != nil {
		return 0, err
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	clear(buf)
	return len(buf), nil
}

func validateSnapshotRunRead(offset, end, innerOffset uint64, length int) error {
	if end <= offset {
		return fmt.Errorf("uffd: invalid snapshot run [%d,%d)", offset, end)
	}
	runLength := end - offset
	if innerOffset > runLength || uint64(length) > runLength-innerOffset {
		return fmt.Errorf("uffd: snapshot run read offset %d length %d outside [0,%d)", innerOffset, length, runLength)
	}
	return nil
}
