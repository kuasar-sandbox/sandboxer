package versioned

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWritePagesForCacheSpaceBoundary(t *testing.T) {
	for _, pressure := range []error{unix.ENOSPC, unix.EDQUOT} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("cached=%t-%v", cached, pressure), func(t *testing.T) {
				s, path := newTestStore(t, 64, nil)
				stopTestGC(s)
				putPages(t, s, []uint64{10}, 1)
				if err := s.Checkpoint(ctx, "retained"); err != nil {
					t.Fatal(err)
				}
				putPages(t, s, []uint64{1, 2}, 2)
				if s.ctrl.Garbage != 0 {
					t.Fatal("fixture has preexisting reclaimable garbage")
				}
				before, controlBefore := s.Stats(), s.ctrl
				denied := 0
				s.beforeIO = func(stage string) error {
					if stage == "reserve" && s.stats.GCFreed == before.GCFreed {
						denied++
						return pressure
					}
					return nil
				}
				pages := []Page{{3, pageFor(3, 3)}}
				write := s.WritePages
				if cached {
					write = s.WritePagesForCache
				}
				err := write(ctx, pages)
				if !errors.Is(err, pressure) || IsSpacePressure(err) != cached {
					t.Fatal("wrong reservation classification", err, s.Err())
				}
				if !cached {
					if !errors.Is(s.Err(), pressure) {
						t.Fatal("ordinary WritePages lost sticky exhaustion", s.Err())
					}
					return
				}
				after := s.Stats()
				if s.Err() != nil || s.ctrl != controlBefore || after.DataWritten != before.DataWritten || after.RedoWritten != before.RedoWritten || after.Commits != before.Commits {
					t.Fatal("deferred failure published Data/redo or poisoned owner", before, after, s.Err())
				}
				checkActive(t, s, map[uint64][]byte{1: pageFor(1, 2), 2: pageFor(2, 2), 10: pageFor(10, 1)})
				if err := s.DiscardPages(ctx, []uint64{1}); err != nil {
					t.Fatal(err)
				}
				if err := write(ctx, pages); err != nil {
					t.Fatal("retry after accepted Zero could not reclaim space", err)
				}
				s.beforeIO = nil
				if denied < 2 || s.Stats().GCFreed == before.GCFreed {
					t.Fatal("did not exercise actual GC assistance", denied, s.Stats())
				}
				want := map[uint64][]byte{1: nil, 2: pageFor(2, 2), 3: pageFor(3, 3), 10: pageFor(10, 1)}
				checkActive(t, s, want)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s, err = Open(path, Options{})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				stopTestGC(s)
				checkActive(t, s, want)
				v, err := s.Export(ctx, "retained")
				if err != nil {
					t.Fatal(err)
				}
				defer v.Close()
				checkView(t, v, map[uint64][]byte{10: pageFor(10, 1)})
			})
		}
	}
}

func TestWritePagesForCacheCommittedPrefixCanReplay(t *testing.T) {
	s, _ := newTestStore(t, 1024, nil)
	stopTestGC(s)
	putPages(t, s, []uint64{1000}, 1)
	if err := s.Checkpoint(ctx, "retained"); err != nil {
		t.Fatal(err)
	}
	var old []uint64
	var pages []Page
	want := map[uint64][]byte{1000: pageFor(1000, 1)}
	for i := uint64(0); i < 2*MaxBatchPages; i++ {
		old = append(old, i+1)
		pages = append(pages, Page{512 + i, pageFor(512+i, 3)})
		want[i+1] = pageFor(i+1, 2)
	}
	putPages(t, s, old, 2)
	before := s.Stats()
	s.beforeIO = func(stage string) error {
		if stage == "reserve" && s.stats.DataWritten-before.DataWritten >= MaxBatchPages*PageSize && s.stats.GCFreed == before.GCFreed {
			return unix.ENOSPC
		}
		return nil
	}
	err := s.WritePagesForCache(ctx, pages)
	if !IsSpacePressure(err) || s.Err() != nil {
		t.Fatal("later Data batch did not defer reservation pressure", err, s.Err())
	}
	for _, p := range pages[:MaxBatchPages] {
		want[p.Block] = p.Data
	}
	checkActive(t, s, want)
	if err := s.DiscardPages(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.WritePagesForCache(ctx, pages); err != nil {
		t.Fatal("complete frozen window could not replay its committed prefix", err)
	}
	s.beforeIO = nil
	for _, b := range old {
		want[b] = nil
	}
	for _, p := range pages {
		want[p.Block] = p.Data
	}
	checkActive(t, s, want)
	v, err := s.Export(ctx, "retained")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, map[uint64][]byte{1000: pageFor(1000, 1)})
}

func TestWritePagesForCacheDoesNotDeferIOOrRollbackFailure(t *testing.T) {
	for _, test := range []struct {
		stage            string
		err              error
		private, cleanup bool
	}{
		{"reserve", unix.EIO, false, false},
		{"data", unix.ENOSPC, false, false},
		{"data", unix.ENOSPC, true, false},
		{"data", unix.EIO, false, false},
		{"redo-images", unix.ENOSPC, false, false},
		{"reserve", unix.ENOSPC, false, true},
	} {
		t.Run(fmt.Sprintf("%s-%v-private=%t-cleanup=%t", test.stage, test.err, test.private, test.cleanup), func(t *testing.T) {
			s, path := newTestStore(t, 64, nil)
			stopTestGC(s)
			putPages(t, s, []uint64{1, 2}, 1)
			want := map[uint64][]byte{1: pageFor(1, 1), 2: pageFor(2, 1)}
			if test.cleanup {
				putPages(t, s, []uint64{50}, 1)
				if err := s.Discard(ctx, 50*PageSize, PageSize); err != nil {
					t.Fatal(err)
				}
				want[50] = nil // retry must not attempt GC after rollback EIO
			}
			before := s.Stats()
			cleanupCalls := 0
			s.beforeIO = func(stage string) error {
				if stage == "rollback-punch" && test.cleanup {
					cleanupCalls++
					return unix.EIO
				}
				if stage == test.stage {
					return test.err
				}
				return nil
			}
			block := uint64(3)
			if test.private {
				block = 1
			}
			err := s.WritePagesForCache(ctx, []Page{{block, pageFor(block, 3)}})
			if IsSpacePressure(err) || !errors.Is(err, test.err) || !errors.Is(s.Err(), test.err) || s.Stats().Commits != before.Commits {
				t.Fatal("storage failure became retryable or published state", err, s.Err(), before, s.Stats())
			}
			if test.cleanup && (cleanupCalls == 0 || !errors.Is(err, unix.EIO) || !errors.Is(s.Err(), unix.EIO) || s.Stats().GCFreed != before.GCFreed) {
				t.Fatal("rollback failure was lost or retried", err, s.Err(), cleanupCalls, before, s.Stats())
			}
			_ = s.Close()
			s, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			stopTestGC(s)
			checkActive(t, s, want)
		})
	}
}
