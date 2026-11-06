package versioned

import "testing"

func TestDiscardPagesRetainedReopen(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plain"
		var key *[32]byte
		if encrypted {
			name, key = "encrypted", &[32]byte{7}
		}
		t.Run(name, func(t *testing.T) {
			s, path := newTestStore(t, 2048, key)
			stopTestGC(s)
			historical := map[uint64][]byte{}
			var data []Page
			for b := uint64(0); b < 1536; b += 2 {
				p := pageFor(b, 1)
				if b == 300 || b == 1024 {
					p = make([]byte, PageSize)
				}
				data = append(data, Page{b, p})
				historical[b] = p
			}
			if err := s.WritePages(ctx, data); err != nil {
				t.Fatal(err)
			}
			if err := s.Checkpoint(ctx, "before"); err != nil {
				t.Fatal(err)
			}
			putPages(t, s, []uint64{0, 2, 30, 510, 1022}, 2)
			var blocks []uint64
			want := snapshotModel(historical)
			for i := MaxDiscardPages - 1; i >= 0; i-- {
				blocks = append(blocks, uint64(i))
				want[uint64(i)] = nil
			}
			before := s.Stats()
			if err := s.DiscardPages(ctx, blocks); err != nil {
				t.Fatal(err)
			}
			if after := s.Stats(); after.Commits != before.Commits+1 || after.DataWritten != before.DataWritten || blocks[0] != MaxDiscardPages-1 {
				t.Fatal("Zero window wrote Data, split a compact tree, or changed input order", before, after)
			}
			checkActive(t, s, want)
			before = s.Stats()
			if err := s.DiscardPages(ctx, blocks); err != nil || s.Stats().Commits != before.Commits {
				t.Fatal("repeated Zero window was not idempotent", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(path, Options{Key: key})
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
		})
	}
}

func TestDiscardPagesCombinesDisjointZeros(t *testing.T) {
	s, _ := newTestStore(t, 2*MaxDiscardPages, nil)
	stopTestGC(s)
	var blocks []uint64
	want := map[uint64][]byte{}
	for i := uint64(0); i < MaxDiscardPages; i++ {
		blocks = append(blocks, 2*i)
		want[2*i] = nil
	}
	putPages(t, s, blocks, 1)
	before := s.Stats()
	if err := s.DiscardPages(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	if s.Stats().Commits != before.Commits+1 || s.Stats().DataWritten != before.DataWritten {
		t.Fatal("disjoint Zeros did not share one metadata transaction", before, s.Stats())
	}
	checkActive(t, s, want)
}

func TestCacheMutationValidationPrecedesCurrentFence(t *testing.T) {
	s, _ := newTestStore(t, MaxDiscardPages+2, nil)
	stopTestGC(s)
	putPages(t, s, []uint64{1}, 1)
	if err := s.Checkpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	before := s.Stats()
	invalidData := [][]Page{{{1, nil}}, {{1, make([]byte, PageSize-1)}}, {{MaxDiscardPages + 2, pageFor(0, 1)}}}
	var oversized []Page
	for b := uint64(0); b <= 4*MaxBatchPages; b++ {
		oversized = append(oversized, Page{b, pageFor(b, 1)})
	}
	invalidData = append(invalidData, oversized)
	late := append([]Page(nil), oversized[:MaxBatchPages+1]...)
	late = append(late, Page{1, nil})
	invalidData = append(invalidData, late)
	for _, pages := range invalidData {
		if err := s.WritePagesForCache(ctx, pages); err == nil || IsSpacePressure(err) {
			t.Fatal("invalid Data window was accepted or classified as space pressure", err)
		}
	}
	invalidZero := [][]uint64{{1, 1}, {MaxDiscardPages + 2}}
	var tooMany []uint64
	for b := uint64(0); b <= MaxDiscardPages; b++ {
		tooMany = append(tooMany, b)
	}
	invalidZero = append(invalidZero, tooMany)
	for _, blocks := range invalidZero {
		if err := s.DiscardPages(ctx, blocks); err == nil || IsSpacePressure(err) {
			t.Fatal("invalid Zero window was accepted", err)
		}
	}
	if err := s.WritePages(ctx, []Page{{1, nil}}); err == nil {
		t.Fatal("ordinary WritePages accepted nil")
	}
	if after := s.Stats(); s.Err() != nil || !s.CanResume("before") || after.Commits != before.Commits || after.DataWritten != before.DataWritten {
		t.Fatal("validation changed state", before, after, s.Err())
	}
	checkActive(t, s, map[uint64][]byte{1: pageFor(1, 1)})
}

func TestDiscardPagesFencesCheckpointAfterInitialMark(t *testing.T) {
	s, _ := newTestStore(t, 64, nil)
	stopTestGC(s)
	putPages(t, s, []uint64{1, 2}, 1)
	gate := &firstErrContext{Context: ctx, fn: func() {
		if err := s.Checkpoint(ctx, "intervening"); err != nil {
			t.Fatal(err)
		}
	}}
	if err := s.DiscardPages(gate, []uint64{3, 1}); err != nil {
		t.Fatal(err)
	}
	if s.CanResume("intervening") {
		t.Fatal("Zero window left a stale Current marker")
	}
	checkActive(t, s, map[uint64][]byte{1: nil, 2: pageFor(2, 1), 3: nil})
	v, err := s.Export(ctx, "intervening")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, map[uint64][]byte{1: pageFor(1, 1), 2: pageFor(2, 1)})
}

func TestDiscardPagesActualRedoBudgetSplits(t *testing.T) {
	s, path, historical, _ := fragmentedDiscardFixture(t)
	if err := s.Checkpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkModified(ctx); err != nil {
		t.Fatal(err)
	}
	var blocks []uint64
	want := snapshotModel(historical)
	for i := MaxDiscardPages - 1; i >= 0; i-- {
		blocks = append(blocks, uint64(i))
		want[uint64(i)] = nil
	}
	var images []uint64
	s.beforeIO = func(stage string) error {
		if stage == "redo-clear" {
			b, err := readFull(s.file, logCommitPage)
			if err != nil {
				return err
			}
			images = append(images, le.Uint64(b[16:]))
		}
		return nil
	}
	if err := s.DiscardPages(ctx, blocks); err != nil {
		t.Fatal(err)
	}
	s.beforeIO = nil
	if len(images) != 2 || images[0] > maxLogPages || images[1] > maxLogPages || s.Err() != nil {
		t.Fatal("unsafe Zero-window redo split", images, s.Err())
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
	if err := s.DiscardPages(ctx, blocks); err != nil {
		t.Fatal("replayed Zero window skipped or double-retired pages", err)
	}
	checkActive(t, s, want)
	v, err := s.Export(ctx, "before")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, historical)
	t.Logf("Zero-window actual budget split: %v", images)
}
