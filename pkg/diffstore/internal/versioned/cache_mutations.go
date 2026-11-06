package versioned

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
)

// SpacePressureError is returned only by WritePagesForCache after an exhausted
// ENOSPC/EDQUOT reservation. The failed 64-page transaction wrote no Data or
// redo and its allocated-slot cleanup succeeded. Earlier Data batches and GC
// assistance may have committed. The Store remains healthy; the cache must
// either finish its bounded recovery protocol or enter its own sticky failure.
type SpacePressureError struct{ Err error }

func (e *SpacePressureError) Error() string {
	return fmt.Sprintf("diffstore: cache space reservation pressure: %v", e.Err)
}
func (e *SpacePressureError) Unwrap() error { return e.Err }

func IsSpacePressure(err error) bool {
	var pressure *SpacePressureError
	return errors.As(err, &pressure) && pressure != nil &&
		(errors.Is(pressure.Err, unix.ENOSPC) || errors.Is(pressure.Err, unix.EDQUOT))
}

// WritePagesForCache uses the ordinary Data path and 64-page transactions, with
// no new redo preflight or uniqueness map. A cache window has at most 256 full
// Data pages. On SpacePressureError its caller keeps all buffers frozen, may
// commit an already accepted finite Zero cohort, and retries the same complete
// Data window once. Reissuing a previously committed Data prefix is safe; no
// operation in this window can independently discard another input page.
// WritePages and WriteAt keep their existing sticky reservation semantics.
func (s *Store) WritePagesForCache(ctx context.Context, pages []Page) error {
	return s.writePages(ctx, pages, true)
}

// DiscardPages commits one bounded, unordered window of distinct logical Zero
// pages. It never writes payload. Metadata pressure may split the window, so
// success covers the whole window but failure can leave a committed prefix.
func (s *Store) DiscardPages(ctx context.Context, blocks []uint64) error {
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	if len(blocks) == 0 {
		return ctx.Err()
	}
	if len(blocks) > MaxDiscardPages {
		return fmt.Errorf("diffstore: Zero window exceeds %d pages", MaxDiscardPages)
	}
	ordered := append([]uint64(nil), blocks...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for i, block := range ordered {
		if block >= uint64(s.Size()/PageSize) || i > 0 && ordered[i-1] == block {
			return fmt.Errorf("diffstore: invalid or repeated block in Zero window")
		}
	}
	if err := s.MarkModified(ctx); err != nil {
		return err
	}
	start, end := 0, len(ordered)
	for start < len(ordered) {
		s.mu.Lock()
		if err := s.healthy(ctx); err != nil {
			s.mu.Unlock()
			return err
		}
		if s.metadataOnly {
			s.mu.Unlock()
			return fmt.Errorf("diffstore: metadata-only handle cannot discard payload")
		}
		err := s.mutateWithRedoBudget(ctx, func(tx *transaction) error {
			tx.ctrl.Current = 0
			for i := start; i < end; {
				j := i + 1
				for j < end && ordered[j] == ordered[j-1]+1 {
					j++
				}
				first, last := ordered[i], ordered[j-1]+1
				if err := s.validatePayloadRange(ctx, first, last); err != nil {
					return err
				}
				tx.setPresenceRange(first, last)
				if err := tx.unsetRange(first, last); err != nil {
					return err
				}
				i = j
			}
			return nil
		}, maxLogPages)
		s.mu.Unlock()
		var budget *discardRedoBudget
		if errors.As(err, &budget) && end-start > 1 {
			end = start + (end-start)/2
			continue
		}
		if err != nil {
			return err
		}
		start, end = end, len(ordered)
	}
	return nil
}
