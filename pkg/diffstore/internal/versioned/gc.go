package versioned

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/directio"
	"golang.org/x/sys/unix"
)

type GCState string

const (
	Complete         GCState = "complete"
	More             GCState = "more"
	BlockedByReaders GCState = "blocked_by_readers"
)

// FreedSlots counts only locations whose free-list publication committed.
// Scans, ownership transfers and uncommitted punches are not freed slots.
type GCResult struct {
	State      GCState `json:"state"`
	FreedSlots uint64  `json:"freed_slots"`
}

// Pure traversal progress is deliberately volatile. A round stops at the tail
// observed when it starts; appends and ordinary writes cannot restart/extend it.
// Deletion invalidates a completed blocked observation, without losing progress
// through a distant predecessor. Previous/Next links never gain older versions.
type gcScan struct {
	cursor, tail, generation, visited, limit, blocked uint64
	predecessor, predecessorFor, predecessorEpoch     uint64
	predecessorReady                                  bool
}

func (s *Store) GC(ctx context.Context, budget int) (GCResult, error) {
	if err := WaitInitialized(s, ctx); err != nil {
		return GCResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcLocked(ctx, min(max(budget, 0), MaxBatchPages))
}

func (s *Store) gcLocked(ctx context.Context, budget int) (result GCResult, err error) {
	result.State = More
	if err = s.healthy(ctx); err != nil {
		return result, err
	}
	if s.ctrl.Garbage == 0 && s.ctrl.Pending == 0 {
		s.gc = gcScan{}
		result.State = Complete
		return result, nil
	}
	if budget == 0 {
		return result, nil
	}
	before := s.stats
	defer func() {
		s.stats.GCRead += s.stats.IndexRead - before.IndexRead
		s.stats.GCWritten += s.stats.IndexWritten - before.IndexWritten + s.stats.RedoWritten - before.RedoWritten
	}()
	// Alternate full batches when both classes are ready. Splitting every batch
	// would commit two half-sized transactions and double metadata overhead.
	// Each class gets a turn even with budget=1 and a growing global chain.
	// Scans, discovery and transfers remain separately bounded by the budget.
	releaseBudget, versionBudget := budget, budget
	if s.ctrl.Garbage != 0 && s.ctrl.Pending != 0 {
		if s.gcVersionTurn {
			releaseBudget = 0
		} else {
			versionBudget = 0
		}
		s.gcVersionTurn = !s.gcVersionTurn
	}
	if s.ctrl.Garbage != 0 && releaseBudget != 0 {
		result.FreedSlots, err = s.gcRelease(ctx, releaseBudget)
		if err != nil {
			return result, err
		}
	}
	blocked := false
	if s.ctrl.Pending != 0 && versionBudget != 0 {
		blocked, err = s.gcVersions(ctx, versionBudget)
		if err != nil {
			return result, err
		}
	}
	if s.ctrl.Garbage == 0 {
		if s.ctrl.Pending == 0 {
			result.State = Complete
			s.gc = gcScan{}
		} else if blocked {
			result.State = BlockedByReaders
		}
	}
	return result, nil
}

func (s *Store) gcRelease(ctx context.Context, budget int) (uint64, error) {
	var slots []uint64
	err := s.mutateOnce(ctx, func(t *transaction) error {
		seen := make(map[uint64]bool, budget)
		next := t.ctrl.Garbage
		for next != 0 && len(slots) < budget {
			if seen[next] {
				return fmt.Errorf("diffstore: duplicate garbage candidate")
			}
			d, err := t.desc(next)
			if err != nil {
				return err
			}
			if d.Kind != kindGarbage {
				return fmt.Errorf("diffstore: invalid garbage chain")
			}
			seen[next] = true
			slots = append(slots, next)
			next = d.Next
		}
		if seen[next] {
			return fmt.Errorf("diffstore: garbage chain cycle")
		}
		sort.Slice(slots, func(i, j int) bool { return slotPage(slots[i]) < slotPage(slots[j]) })
		// Adjacent slot numbers can straddle a descriptor page. Only physical
		// page adjacency permits merging; every punched page is a validated slot.
		for start := 0; start < len(slots); {
			end := start + 1
			for end < len(slots) && slotPage(slots[end]) == slotPage(slots[end-1])+1 {
				end++
			}
			if err := s.hook("gc-punch"); err != nil {
				return err
			}
			length := int64(end-start) * PageSize
			if err := directio.PunchHole(int(s.file.Fd()),
				int64(slotPage(slots[start]))*PageSize, length); err != nil {
				return err
			}
			s.stats.GCPunched += uint64(length)
			s.stats.GCPunchCalls++
			start = end
		}
		if err := s.hook("gc-punched"); err != nil {
			return err
		}
		// Publish the ascending batch as one transaction, followed by the old
		// free list. No punched slot is reusable before this transaction commits.
		for i, slot := range slots {
			d, err := t.desc(slot)
			if err != nil {
				return err
			}
			d.Kind, d.Next = kindFree, t.ctrl.Free
			if i+1 < len(slots) {
				d.Next = slots[i+1]
			}
			if err := t.setDesc(slot, d); err != nil {
				return err
			}
			s.cache.remove(slotPage(slot))
		}
		t.ctrl.Garbage = next
		if len(slots) != 0 {
			t.ctrl.Free = slots[0]
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.stats.GCProcessed += uint64(len(slots))
	s.stats.GCFreed += uint64(len(slots))
	return uint64(len(slots)), nil
}

func (s *Store) gcVersions(ctx context.Context, budget int) (bool, error) {
	g := s.gc
	if g.cursor == 0 {
		g = gcScan{cursor: s.ctrl.Head, tail: s.ctrl.Tail, generation: s.gcDeletion, limit: s.ctrl.Next}
	}
	blocked, moved := false, uint64(0)
	err := s.mutateOnce(ctx, func(t *transaction) error {
		visits, discovery, transfers := 0, 0, 0
		for g.cursor != 0 && visits < budget {
			if err := ctx.Err(); err != nil {
				return err
			}
			if g.visited >= g.limit {
				return fmt.Errorf("diffstore: GC checkpoint chain cycle")
			}
			cp, err := s.readCheckpoint(g.cursor, t)
			if err != nil {
				return err
			}
			original := cp
			visits++
			if cp.Pending && cp.Retained {
				err = ofd(s.lock, unix.F_WRLCK, checkpointLockBase+int64(g.cursor), false)
				if err == nil {
					if err = ofd(s.lock, unix.F_UNLCK, checkpointLockBase+int64(g.cursor), false); err != nil {
						return err
					}
					cp.Retained = false
				} else if errors.Is(err, ErrBusy) {
					g.blocked++
				} else {
					return err
				}
			}
			if cp.Pending && !cp.Retained {
				released, err := t.releaseBitmap(&cp, budget-transfers)
				if err != nil {
					return err
				}
				transfers += released
				moved += uint64(released)
				if g.predecessorFor != g.cursor {
					g.predecessorFor, g.predecessor = g.cursor, cp.Previous
					g.predecessorEpoch = cp.Epoch
					g.predecessorReady = cp.Garbage == 0 && t.ctrl.Protected != g.cursor
				}
				var previous checkpoint
				// Revalidate a remembered retained predecessor: an independent
				// Delete may have withdrawn it since the preceding batch.
				if g.predecessorReady && g.predecessor != 0 && (cp.Garbage != 0 || t.ctrl.Protected == g.cursor) {
					previous, err = s.readCheckpoint(g.predecessor, t)
					if err != nil {
						return err
					}
					if !previous.Retained {
						g.predecessorReady = false
					}
				}
				for !g.predecessorReady && g.predecessor != 0 && discovery < budget {
					previous, err = s.readCheckpoint(g.predecessor, t)
					if err != nil {
						return err
					}
					discovery++
					if previous.Epoch >= g.predecessorEpoch {
						return fmt.Errorf("diffstore: invalid GC predecessor")
					}
					if previous.Retained {
						g.predecessorReady = true
						break
					}
					g.predecessorEpoch = previous.Epoch
					g.predecessor = previous.Previous
				}
				if g.predecessor == 0 {
					g.predecessorReady = true
				}
				if g.predecessorReady {
					if t.ctrl.Protected == g.cursor {
						t.ctrl.Protected = g.predecessor
					}
					previousChanged := false
					for cp.Garbage != 0 && transfers < budget {
						slot := cp.Garbage
						d, err := t.desc(slot)
						if err != nil {
							return err
						}
						if d.Kind != kindGarbage || d.Next == slot {
							return fmt.Errorf("diffstore: invalid retired chain")
						}
						cp.Garbage = d.Next
						if g.predecessor != 0 && previous.Epoch >= d.Birth {
							d.Next, previous.Garbage = previous.Garbage, slot
							previousChanged = true
						} else {
							d.Next, t.ctrl.Garbage = t.ctrl.Garbage, slot
						}
						if err := t.setDesc(slot, d); err != nil {
							return err
						}
						transfers++
						moved++
					}
					if previousChanged {
						if err := t.putCheckpoint(g.predecessor, previous); err != nil {
							return err
						}
					}
					if cp.Garbage == 0 && cp.Bitmap == 0 {
						cp.Pending = false
						t.ctrl.Pending--
					}
				}
			}
			if cp != original {
				if err := t.putCheckpoint(g.cursor, cp); err != nil {
					return err
				}
			}
			if cp.Pending && !cp.Retained {
				break
			}
			g.visited++
			if g.cursor == g.tail {
				blocked = g.generation == s.gcDeletion && g.blocked == t.ctrl.Pending && t.ctrl.Pending != 0
				g = gcScan{}
				break
			}
			if cp.Next == 0 {
				return fmt.Errorf("diffstore: GC round lost its tail")
			}
			g.cursor = cp.Next
			g.predecessorFor, g.predecessorReady = 0, false
		}
		return nil
	})
	if err == nil {
		s.gc = g
		s.stats.GCProcessed += moved
	}
	return blocked, err
}
