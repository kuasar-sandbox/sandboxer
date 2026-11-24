package versioned

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func (s *Store) findCheckpoint(id string, t *transaction) (uint64, checkpoint, error) {
	head := s.ctrl.Head
	next := s.ctrl.Next
	if t != nil {
		head = t.ctrl.Head
		next = t.ctrl.Next
	}
	for slot, count := head, uint64(0); slot != 0; count++ {
		if count >= next || slot >= next {
			return 0, checkpoint{}, fmt.Errorf("diffstore: checkpoint chain cycle")
		}
		cp, err := s.readCheckpoint(slot, t)
		if err != nil {
			return 0, checkpoint{}, err
		}
		if cp.ID == id {
			return slot, cp, nil
		}
		slot = cp.Next
	}
	return 0, checkpoint{}, ErrNotExist
}
func (s *Store) Checkpoint(ctx context.Context, id string) error {
	return s.checkpoint(ctx, id, false)
}

func (s *Store) TemporaryCheckpoint(ctx context.Context, id string) error {
	return s.checkpoint(ctx, id, true)
}

func (s *Store) checkpoint(ctx context.Context, id string, temporary bool) error {
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	if err := ValidID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(ctx); err != nil {
		return err
	}
	if _, _, err := s.findCheckpoint(id, nil); err == nil {
		return ErrExists
	} else if !errors.Is(err, ErrNotExist) {
		return s.fail(err)
	}
	return s.mutate(ctx, func(t *transaction) error {
		slot, err := t.allocate(kindCheckpoint)
		if err != nil {
			return err
		}
		bitmap, err := t.snapshotPresence()
		if err != nil {
			return err
		}
		cp := checkpoint{Temporary: temporary, Bitmap: bitmap, ID: id, Epoch: t.ctrl.Epoch, Root: t.ctrl.Root, Previous: t.ctrl.Tail, Visible: true, Retained: true}
		if t.ctrl.Tail != 0 {
			last, err := s.readCheckpoint(t.ctrl.Tail, t)
			if err != nil {
				return err
			}
			last.Next = slot
			if err = t.putCheckpoint(t.ctrl.Tail, last); err != nil {
				return err
			}
		} else {
			t.ctrl.Head = slot
		}
		if err = t.putCheckpoint(slot, cp); err != nil {
			return err
		}
		t.ctrl.Tail = slot
		t.ctrl.Current = slot
		t.ctrl.Protected = slot
		t.ctrl.Epoch++
		if t.ctrl.Epoch == 0 {
			return fmt.Errorf("diffstore: generation exhausted")
		}
		return nil
	})
}
func (s *Store) ListCheckpoints(ctx context.Context) ([]string, error) {
	if err := WaitInitialized(s, ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(ctx); err != nil {
		return nil, err
	}
	out := []string{}
	for slot, count := s.ctrl.Head, uint64(0); slot != 0; count++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if count >= s.ctrl.Next || slot >= s.ctrl.Next {
			return nil, s.fail(fmt.Errorf("diffstore: checkpoint chain cycle"))
		}
		cp, err := s.readCheckpoint(slot, nil)
		if err != nil {
			return nil, s.fail(err)
		}
		if cp.Visible {
			out = append(out, cp.ID)
		}
		slot = cp.Next
	}
	return out, nil
}
func (s *Store) DeleteCheckpoint(ctx context.Context, id string) error {
	return s.DeleteCheckpoints(ctx, []string{id})
}
func (s *Store) DeleteCheckpoints(ctx context.Context, ids []string) error {
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	for _, id := range ids {
		if err := ValidID(id); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.healthy(ctx); err != nil {
		return err
	}
	// Hold the in-process gate across withdrawal of the complete chosen set.
	// Background GC cannot repeatedly transfer a chain through members of it.
	for start := 0; start < len(ids); start += MaxBatchPages {
		part := ids[start:min(start+MaxBatchPages, len(ids))]
		err := s.mutate(ctx, func(t *transaction) error {
			for _, id := range part {
				slot, cp, err := s.findCheckpoint(id, t)
				if err != nil {
					return err
				}
				if !cp.Visible {
					continue
				}
				cp.Visible = false
				s.gcDeletion++ // invalidate an older blocked scan, even on a partial batch
				cp.Pending = true
				t.ctrl.Pending++
				// Withdraw all unpinned members before any GC, including when the
				// persisted cursor starts in the middle of this batch. Pinned deleted
				// versions remain retained and continue to force CoW.
				pinErr := ofd(s.lock, unix.F_WRLCK, checkpointLockBase+int64(slot), false)
				if pinErr == nil {
					_ = ofd(s.lock, unix.F_UNLCK, checkpointLockBase+int64(slot), false)
					cp.Retained = false
				} else if !errors.Is(pinErr, ErrBusy) {
					return pinErr
				}
				if t.ctrl.Current == slot {
					t.ctrl.Current = 0
				}
				if err = t.putCheckpoint(slot, cp); err != nil {
					return err
				}
				if t.ctrl.Protected == slot && !cp.Retained {
					prev, _, err := t.predecessor(cp.Previous)
					if err != nil {
						return err
					}
					t.ctrl.Protected = prev
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
func (t *transaction) predecessor(slot uint64) (uint64, checkpoint, error) {
	for count := uint64(0); slot != 0; count++ {
		if count >= t.ctrl.Next {
			return 0, checkpoint{}, fmt.Errorf("diffstore: checkpoint predecessor cycle")
		}
		cp, err := t.store.readCheckpoint(slot, t)
		if err != nil {
			return 0, checkpoint{}, err
		}
		if cp.Retained {
			return slot, cp, nil
		}
		slot = cp.Previous
	}
	return 0, checkpoint{}, nil
}

// Retire marks the whole unmounted DIFF unavailable before trying to unlink it.
// Existing fixed views retain their inode and return ErrBusy here until closed.
// OpenMetadata can reopen a retired DIFF to retry this exact operation.
func (s *Store) Retire(ctx context.Context) error {
	if err := WaitInitialized(s, ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	if s.fatal != nil {
		return s.fatal
	}
	if !s.ctrl.Retired {
		if err := s.mutate(ctx, func(t *transaction) error { t.ctrl.Retired = true; t.ctrl.Current = 0; return nil }); err != nil {
			return err
		}
	}
	if err := ofd(s.lock, unix.F_WRLCK, readerByte, false); err != nil {
		return err
	}
	defer ofd(s.lock, unix.F_UNLCK, readerByte, false)
	// A template owner can retain its original descriptor across publication by
	// rename. Resolve that inode's current name, then check it before unlinking.
	path, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", s.file.Fd()))
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	owned, err := s.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(current, owned) {
		return fmt.Errorf("diffstore: retired path no longer identifies the owned inode")
	}
	return os.Remove(path)
}

// The exclusive owner lock is held by openStore. Reader pins from another
// process still protect deleted versions through the ordinary GC mechanism.
func (s *Store) recoverTemporaryCheckpoints(ctx context.Context) error {
	var ids []string
	for slot, count := s.ctrl.Head, uint64(0); slot != 0; count++ {
		if count >= s.ctrl.Next || slot >= s.ctrl.Next {
			return fmt.Errorf("diffstore: checkpoint chain cycle")
		}
		cp, err := s.readCheckpoint(slot, nil)
		if err != nil {
			return err
		}
		if cp.Temporary && cp.Visible {
			ids = append(ids, cp.ID)
		}
		slot = cp.Next
	}
	if len(ids) == 0 {
		return nil
	}
	return s.DeleteCheckpoints(ctx, ids)
}
