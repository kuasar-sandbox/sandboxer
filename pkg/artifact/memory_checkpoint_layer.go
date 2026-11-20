package artifact

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// PortableMemoryLayer turns one logical memory lower/import into an ordinary
// Snapshot without expanding the caller's external layer list. A capture root
// uses materializeMemoryCheckpointRoot instead, preserving its separate W/H.
// This lower retains all visible values and labels their exact dirty union.
// It takes ownership of root on success and failure.
func PortableMemoryLayer(ctx context.Context, root *snapshotfile.Root) (fetch.Stream, error) {
	if root == nil {
		return nil, errors.New("portable memory layer: nil root")
	}
	if root.LocalCheckpoint == nil {
		return root.FullStream, nil
	}
	dirty, err := portableMemoryDirty(ctx, root.Memory, root.LocalCheckpoint.Dirty)
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	source, err := snapshotfile.BuildSourceWithDirty(root.Memory, root.ConfigJSON, root.StateJSON, root.SnapshotConfig, dirty)
	if dirty != nil && errors.Is(err, snapshotfile.ErrTailBudget) {
		// The existing format's explicit fallback is all-dirty; it never drops
		// a value or promotes a Hole into a zero-filled memory page.
		source, err = snapshotfile.BuildSource(root.Memory, root.ConfigJSON, root.StateJSON, root.SnapshotConfig)
	}
	if err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return &portableMemoryStream{Source: source, root: root}, nil
}

type portableMemoryStream struct {
	sparse.Source
	root *snapshotfile.Root
}

func (s *portableMemoryStream) Close() error { return s.root.Close() }

// A bitmap can describe only full pages. Legacy partial-byte dirty runs are
// preserved exactly by omitting the optional bitmap (all explicit dirty).
func portableMemoryDirty(ctx context.Context, values, dirty sparse.Source) ([]byte, error) {
	if values == nil || dirty == nil || values.Size() != dirty.Size() || values.Size()%snapshotfile.DirtyPageSize != 0 {
		return nil, errors.New("portable memory layer: invalid candidate geometry")
	}
	pageSize := snapshotfile.DirtyPageSize
	bits := make([]byte, (values.Size()/pageSize+7)/8)
	clean, partial := false, false
	var start, end uint64
	mark := func() {
		if start == end {
			return
		}
		if start%pageSize != 0 || end%pageSize != 0 {
			partial = true
			return
		}
		for page := start / pageSize; page < end/pageSize; page++ {
			bits[page/8] |= 1 << (page % 8)
		}
	}
	for off := uint64(0); off < values.Size(); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value, err := values.RunAt(off, values.Size()-off)
		if err != nil {
			return nil, err
		}
		if value == nil || value.Offset() != off || value.End() <= off || value.End() > values.Size() || value.Kind() > sparse.Data {
			return nil, errors.New("portable memory layer: invalid value run")
		}
		candidate, err := dirty.RunAt(off, value.End()-off)
		if err != nil {
			return nil, err
		}
		if candidate == nil || candidate.Offset() != off || candidate.End() <= off || candidate.End() > value.End() || candidate.Kind() > sparse.Data {
			return nil, errors.New("portable memory layer: invalid candidate run")
		}
		if candidate.Kind() != sparse.Hole {
			if value.Kind() == sparse.Hole {
				return nil, fmt.Errorf("portable memory layer: candidate covers Hole at %d", off)
			}
			if start == end {
				start = off
			}
			end = candidate.End()
		} else {
			mark()
			start, end = candidate.End(), candidate.End()
			clean = clean || value.Kind() != sparse.Hole
		}
		off = candidate.End()
	}
	mark()
	if partial || !clean {
		return nil, nil
	}
	return snapshotfile.EncodeDirty(values.Size(), bits)
}
