package versioned

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMutationCancellationBeforeCommitLeavesOwnerHealthy(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "metadata"
		if private {
			name = "private-overwrite"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := newTestStore(t, 64, nil)
			stopTestGC(s)
			putPages(t, s, []uint64{1}, 1)
			request, cancel := context.WithCancel(ctx)
			defer cancel()
			before := s.Stats()
			controlBefore := s.ctrl
			err := func() error {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.mutateOnce(request, func(tx *transaction) error {
					slot, err := lookup(tx, tx.ctrl.Root, 1)
					if err != nil {
						return err
					}
					if !private {
						if err := tx.retire(slot); err != nil {
							return err
						}
						slot, err = tx.allocate(kindData)
						if err != nil {
							return err
						}
						if err := tx.set(1, slot); err != nil {
							return err
						}
					}
					tx.data = append(tx.data, dataWrite{slot, 1, pageFor(1, 2)})
					cancel() // The prepared mutation must not reach payload/redo I/O.
					return nil
				})
			}()
			if !errors.Is(err, context.Canceled) || s.Err() != nil {
				t.Fatalf("request=%v owner=%v", err, s.Err())
			}
			after := s.Stats()
			if s.ctrl != controlBefore || after.DataWritten != before.DataWritten ||
				after.IndexWritten != before.IndexWritten || after.RedoWritten != before.RedoWritten ||
				after.Commits != before.Commits {
				t.Fatalf("canceled transaction published state: before=%+v after=%+v", before, after)
			}
			checkActive(t, s, map[uint64][]byte{1: pageFor(1, 1)})
			if err := s.Checkpoint(ctx, "after-cancel"); err != nil {
				t.Fatal("fresh checkpoint failed", err)
			}
			putPages(t, s, []uint64{1}, 3)
			view, err := s.Export(ctx, "after-cancel")
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			checkView(t, view, map[uint64][]byte{1: pageFor(1, 1)})
			checkActive(t, s, map[uint64][]byte{1: pageFor(1, 3)})
		})
	}
}

// Capture Err before notifying the test, so its first check deterministically
// succeeds even if cancellation races the return into the metadata lock wait.
type mutationEntryContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *mutationEntryContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.entered) })
	return err
}

func TestMutationRechecksCancellationAfterMetadataLock(t *testing.T) {
	s, path := newTestStore(t, 64, nil)
	stopTestGC(s)
	lock, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := ofd(lock, unix.F_WRLCK, metadataByte, false); err != nil {
		t.Fatal(err)
	}
	defer ofd(lock, unix.F_UNLCK, metadataByte, false)
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	gate := &mutationEntryContext{Context: request, entered: make(chan struct{})}
	done := make(chan error, 1)
	prepared := false
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		done <- s.mutateOnce(gate, func(tx *transaction) error {
			prepared = true
			tx.ctrl.Epoch++
			return nil
		})
	}()
	<-gate.entered
	cancel()
	if err := ofd(lock, unix.F_UNLCK, metadataByte, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) || prepared || s.Err() != nil {
		t.Fatalf("request=%v prepared=%t owner=%v", err, prepared, s.Err())
	}
}

func TestAdmittedCommitReturnsActualResultAfterCancellation(t *testing.T) {
	stages := []string{"reserve", "data", "new-index", "redo-reset", "redo-index", "redo-images", "redo-commit", "apply", "redo-clear"}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			s, _ := newTestStore(t, 64, nil)
			stopTestGC(s)
			request, cancel := context.WithCancel(ctx)
			defer cancel()
			fired := false
			s.beforeIO = func(actual string) error {
				if actual == stage {
					fired = true
					cancel()
				}
				return nil
			}
			err := s.WritePages(request, []Page{{1, pageFor(1, 2)}})
			if !fired || err != nil || s.Err() != nil {
				t.Fatalf("canceled=%t request=%v owner=%v", fired, err, s.Err())
			}
			checkActive(t, s, map[uint64][]byte{1: pageFor(1, 2)})
		})
	}
}

func TestCommitIOFailureWithCancellationRemainsFatal(t *testing.T) {
	for _, stage := range []string{"data", "apply"} {
		t.Run(stage, func(t *testing.T) {
			s, _ := newTestStore(t, 64, nil)
			stopTestGC(s)
			s.beforeIO = func(actual string) error {
				if actual == stage {
					return errors.Join(context.Canceled, unix.EIO)
				}
				return nil
			}
			err := s.WritePages(ctx, []Page{{1, pageFor(1, 2)}})
			if !errors.Is(err, unix.EIO) || !errors.Is(s.Err(), unix.EIO) {
				t.Fatalf("commit I/O failure lost: request=%v owner=%v", err, s.Err())
			}
		})
	}
}

func TestGCCancellationAfterPunchCanRetry(t *testing.T) {
	s, _, slots := gcPhysicalFixture(t)
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	before := s.ctrl
	fired := false
	s.beforeIO = func(stage string) error {
		if stage == "gc-punched" {
			fired = true
			cancel()
		}
		return nil
	}
	result, err := s.GC(request, MaxBatchPages)
	if !fired || !errors.Is(err, context.Canceled) || s.Err() != nil || result.FreedSlots != 0 || s.ctrl != before {
		t.Fatalf("punched=%t result=%+v request=%v owner=%v", fired, result, err, s.Err())
	}
	s.beforeIO = nil
	if result := finishTestGC(t, s, MaxBatchPages); result.State != Complete || result.FreedSlots != uint64(len(slots)) {
		t.Fatalf("canceled GC could not retry: %+v", result)
	}
}
