package versioned

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

// This is an accepted sparse B+ layout, with no minimum node occupancy rule.
// Each Data record and each one-entry leaf occupies a separate descriptor
// group. Every other slot is valid Free and remains linked to the allocator.
// A 4 MiB logical range therefore touches more than the real 1536-page redo
// capacity, without allocating a GiB of payload to manufacture fragmentation.
func fragmentedDiscardFixture(t *testing.T) (*Store, string, map[uint64][]byte, []uint64) {
	t.Helper()
	const count = MaxDiscardPages + 1 // the final page is an adjacent live guard
	parentCount := (count + maxEntries - 1) / maxEntries
	groups := count*2 + parentCount + 1
	s, path := newTestStore(t, count+1, nil)
	stopTestGC(s)
	if err := s.file.Truncate(int64(slotPage(uint64(groups*slotsPerGroup))+1) * PageSize); err != nil {
		t.Fatal(err)
	}
	want := map[uint64][]byte{}
	payloads := make([]uint64, count)
	groupSlot := func(group int) uint64 { return uint64(group*slotsPerGroup + 1) }
	s.mu.Lock()
	err := s.mutate(ctx, func(tx *transaction) error {
		tx.ctrl.Next = uint64(groups*slotsPerGroup) + 1
		tx.ctrl.Free = 2
		for g := 0; g < groups; g++ {
			page := descriptorPage(groupSlot(g))
			b := make([]byte, PageSize)
			copy(b, "KDDESC4")
			for n := 1; n <= slotsPerGroup; n++ {
				slot := uint64(g*slotsPerGroup + n)
				d := descriptor{Kind: kindFree}
				if n == 1 {
					d.Birth, d.Kind = tx.ctrl.Epoch, kindIndex
					if g < count {
						d.Kind = kindData
					}
					tx.fresh[slotPage(slot)] = true
					tx.allocated = append(tx.allocated, slot)
				} else if n < slotsPerGroup {
					d.Next = slot + 1
				} else if g+1 < groups {
					d.Next = groupSlot(g+1) + 1
				}
				o := descriptorOffset(slot)
				le.PutUint64(b[o:], d.Birth)
				le.PutUint64(b[o+8:], d.Next)
				le.PutUint64(b[o+16:], d.Kind)
			}
			sealPage(b)
			tx.pages[page], tx.fresh[page] = b, true
		}
		var leaves []entry
		for i := 0; i < count; i++ {
			block := uint64(i)
			slot := groupSlot(i)
			data := pageFor(block, 7)
			want[block], payloads[i] = data, slot
			tx.data = append(tx.data, dataWrite{slot, block, data})
			leaf := groupSlot(count + i)
			tx.nodes[leaf] = node{leaf: true, entries: []entry{{block, slot}}}
			leaves = append(leaves, entry{block, leaf})
		}
		var parents []entry
		for i := 0; i < parentCount; i++ {
			first, last := i*maxEntries, min((i+1)*maxEntries, len(leaves))
			slot := groupSlot(count*2 + i)
			tx.nodes[slot] = node{entries: append([]entry(nil), leaves[first:last]...)}
			parents = append(parents, entry{leaves[last-1].key, slot})
		}
		tx.ctrl.Root = groupSlot(groups - 1)
		tx.nodes[tx.ctrl.Root] = node{entries: parents}
		tx.setPresenceRange(0, count)
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Force all subsequent validation to use the persisted sparse file, before
	// checkpointing or attempting the large discard.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal("reopen fragmented fixture", err)
	}
	t.Cleanup(func() { s.Close() })
	stopTestGC(s)
	checkFragmentedOwnership(t, s)
	if err := s.Scan(ctx, func(uint64) error { return nil }); err != nil {
		t.Fatal("invalid fragmented fixture", err)
	}
	checkActive(t, s, want)
	return s, path, want, payloads
}

// Validate every descriptor CRC and account for every allocated slot through
// either the live tree or the free chain. The sparse gaps contain no invented
// allocated records, omitted descriptors, or unreachable payload ownership.
func checkFragmentedOwnership(t *testing.T, s *Store) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	descriptors := make([]descriptor, s.ctrl.Next)
	for first := uint64(1); first < s.ctrl.Next; first += slotsPerGroup {
		b, err := readFull(s.file, descriptorPage(first))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkPage(b); err != nil || string(b[:7]) != "KDDESC4" {
			t.Fatal("invalid fixture descriptor", first, err)
		}
		for slot := first; slot < min(first+slotsPerGroup, s.ctrl.Next); slot++ {
			o := descriptorOffset(slot)
			descriptors[slot] = descriptor{le.Uint64(b[o:]), le.Uint64(b[o+8:]), le.Uint64(b[o+16:])}
		}
	}
	owned := make([]bool, s.ctrl.Next)
	claim := func(slot, kind uint64) {
		if slot == 0 || slot >= s.ctrl.Next || owned[slot] || descriptors[slot].Kind != kind {
			t.Fatal("invalid or multiply owned fixture slot", slot, kind)
		}
		owned[slot] = true
	}
	var visit func(uint64, int)
	visit = func(slot uint64, depth int) {
		if depth >= maxDepth {
			t.Fatal("fixture tree too deep")
		}
		claim(slot, kindIndex)
		n, err := inspectNode(storeReader{s}, slot)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range n.entries {
			if n.leaf {
				claim(e.slot, kindData)
			} else {
				visit(e.slot, depth+1)
			}
		}
	}
	visit(s.ctrl.Root, 0)
	for slot := s.ctrl.Free; slot != 0; slot = descriptors[slot].Next {
		claim(slot, kindFree)
	}
	for slot := uint64(1); slot < s.ctrl.Next; slot++ {
		if !owned[slot] {
			t.Fatal("unowned allocated fixture slot", slot, descriptors[slot])
		}
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(s.file.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	t.Logf("fragmented fixture: %d slots, logical file %d bytes, allocated %d bytes", s.ctrl.Next-1, st.Size, st.Blocks*512)
}

// Probe the exact prepared page count using a deliberately insufficient
// private budget. No reserve, write, free-list publication, or owner failure
// is permitted; metadata reads and cache warming are allowed.
func probeDiscardRedoPages(t *testing.T, s *Store, start, count uint64) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	before, controlBefore := s.stats, s.ctrl
	presenceBefore := append([]byte(nil), s.presence...)
	var fileBefore, fileAfter unix.Stat_t
	if err := unix.Fstat(int(s.file.Fd()), &fileBefore); err != nil {
		t.Fatal(err)
	}
	oldHook, ioCalls := s.beforeIO, 0
	s.beforeIO = func(stage string) error {
		ioCalls++
		return fmt.Errorf("unexpected preflight I/O at %s", stage)
	}
	defer func() { s.beforeIO = oldHook }()
	err := s.mutateOnceWithRedoBudget(ctx, func(tx *transaction) error {
		tx.ctrl.Current = 0
		if err := s.validatePayloadRange(ctx, start, start+count); err != nil {
			return err
		}
		tx.setPresenceRange(start, start+count)
		return tx.unsetRange(start, start+count)
	}, 1)
	var budget *discardRedoBudget
	if !errors.As(err, &budget) || budget.pages <= budget.limit || s.Err() != nil {
		t.Fatalf("request=%v owner=%v", err, s.Err())
	}
	after := s.stats
	if err := unix.Fstat(int(s.file.Fd()), &fileAfter); err != nil {
		t.Fatal(err)
	}
	if ioCalls != 0 || s.ctrl != controlBefore || !bytes.Equal(s.presence, presenceBefore) ||
		before.DataWritten != after.DataWritten || before.IndexWritten != after.IndexWritten ||
		before.RedoWritten != after.RedoWritten || before.Commits != after.Commits ||
		before.Allocated != after.Allocated || before.Reused != after.Reused ||
		fileBefore.Size != fileAfter.Size || fileBefore.Blocks != fileAfter.Blocks {
		t.Fatalf("budget rejection published or reserved state: calls=%d before=%+v after=%+v", ioCalls, before, after)
	}
	return budget.pages
}

func TestDiscardActualRedoBudgetSplitsFragmentedRange(t *testing.T) {
	s, path, historical, payloads := fragmentedDiscardFixture(t)
	if err := s.Checkpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkModified(ctx); err != nil {
		t.Fatal(err)
	}
	full := probeDiscardRedoPages(t, s, 0, MaxDiscardPages)
	half := probeDiscardRedoPages(t, s, 0, MaxDiscardPages/2)
	if full <= maxLogPages || half > maxLogPages {
		t.Fatalf("fixture did not straddle actual redo capacity: full=%d half=%d capacity=%d", full, half, maxLogPages)
	}
	checkActive(t, s, historical) // rejected preparation must not alias cached nodes
	var committedPages []uint64
	s.beforeIO = func(stage string) error {
		if stage == "redo-clear" {
			b, err := readFull(s.file, logCommitPage)
			if err != nil {
				return err
			}
			committedPages = append(committedPages, le.Uint64(b[16:]))
		}
		return nil
	}
	if err := s.Discard(ctx, 0, MaxDiscardPages*PageSize); err != nil {
		t.Fatal(err)
	}
	s.beforeIO = nil
	if len(committedPages) != 2 || committedPages[0] != uint64(half) {
		t.Fatalf("unexpected split or preflight count: full=%d half=%d commits=%v", full, half, committedPages)
	}
	for _, count := range committedPages {
		if count > maxLogPages {
			t.Fatal("oversized redo committed", count)
		}
	}
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	want := snapshotModel(historical)
	for b := uint64(0); b < MaxDiscardPages; b++ {
		want[b] = nil
	}
	checkActive(t, s, want)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stopTestGC(s)
	checkActive(t, s, want)
	v, err := s.Export(ctx, "before")
	if err != nil {
		t.Fatal(err)
	}
	checkView(t, v, historical)
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCheckpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	if result := finishTestGC(t, s, 64); result.State != Complete {
		t.Fatal(result)
	}
	checkActive(t, s, want)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &transaction{store: s, ctrl: s.ctrl}
	for _, slot := range payloads[:MaxDiscardPages] {
		d, err := tx.desc(slot)
		if err != nil || d.Kind != kindFree {
			t.Fatal("split discard lost payload retirement", slot, d, err)
		}
	}
	t.Logf("actual redo preflight: %d pages > %d; committed images after split: %v", full, maxLogPages, committedPages)
}

func denseDiscardBudgetFixture(t *testing.T) (*Store, string, map[uint64][]byte, int) {
	t.Helper()
	s, path := newTestStore(t, MaxDiscardPages+64, nil)
	stopTestGC(s)
	want := map[uint64][]byte{}
	var pages []Page
	for b := uint64(0); b < MaxDiscardPages+1; b++ {
		data := pageFor(b, 9)
		pages = append(pages, Page{b, data})
		want[b] = data
	}
	if err := s.WritePages(ctx, pages); err != nil {
		t.Fatal(err)
	}
	// Leave private garbage for the existing bounded ENOSPC assistance path.
	garbageBlock := uint64(MaxDiscardPages + 32)
	putPages(t, s, []uint64{garbageBlock}, 1)
	if err := s.Discard(ctx, int64(garbageBlock)*PageSize, PageSize); err != nil {
		t.Fatal(err)
	}
	want[garbageBlock] = nil
	if err := s.Checkpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkModified(ctx); err != nil {
		t.Fatal(err)
	}
	full := probeDiscardRedoPages(t, s, 0, MaxDiscardPages)
	half := probeDiscardRedoPages(t, s, 0, MaxDiscardPages/2)
	if half >= full {
		t.Fatalf("dense fixture has no useful split: full=%d half=%d", full, half)
	}
	return s, path, want, full - 1
}

func TestDiscardBudgetSplitSurvivesReservationRetry(t *testing.T) {
	s, _, historical, budget := denseDiscardBudgetFixture(t)
	fired := false
	s.beforeIO = func(stage string) error {
		if stage == "reserve" && !fired {
			fired = true
			return unix.ENOSPC
		}
		return nil
	}
	before := s.Stats()
	if err := s.discardWithRedoBudget(ctx, 0, MaxDiscardPages*PageSize, budget); err != nil {
		t.Fatal(err)
	}
	s.beforeIO = nil
	if !fired || s.Stats().GCFreed == before.GCFreed || s.Err() != nil {
		t.Fatal("split did not survive real reservation retry", fired, s.Stats(), s.Err())
	}
	want := snapshotModel(historical)
	for b := uint64(0); b < MaxDiscardPages; b++ {
		want[b] = nil
	}
	checkActive(t, s, want)
	v, err := s.Export(ctx, "before")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, historical)
}

func TestDiscardBudgetSplitReplaysAndCanResumeWholeRange(t *testing.T) {
	s, path, historical, budget := denseDiscardBudgetFixture(t)
	s.beforeIO = func(stage string) error {
		if stage == "apply" {
			return unix.EIO
		}
		return nil
	}
	err := s.discardWithRedoBudget(ctx, 0, MaxDiscardPages*PageSize, budget)
	if !errors.Is(err, unix.EIO) || !errors.Is(s.Err(), unix.EIO) {
		t.Fatal("commit I/O failure was treated as a retryable budget result", err, s.Err())
	}
	_ = s.Close()
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stopTestGC(s)
	want := snapshotModel(historical)
	for b := uint64(0); b < MaxDiscardPages/2; b++ {
		want[b] = nil
	}
	checkActive(t, s, want)
	if err := s.discardWithRedoBudget(ctx, 0, MaxDiscardPages*PageSize, budget); err != nil {
		t.Fatal("replaying the complete request skipped or double-retired pages", err)
	}
	for b := uint64(MaxDiscardPages / 2); b < MaxDiscardPages; b++ {
		want[b] = nil
	}
	checkActive(t, s, want)
	v, err := s.Export(ctx, "before")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, historical)
}
