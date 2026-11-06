package versioned

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
)

// Exercise range removal through leaf boundaries with both protected and
// private payloads. The content oracle includes Hole, explicit Zero, and a
// physically present all-zero Data page; the free-list check detects payload
// slots accidentally detached from the tree without being retired.
func TestDiscardRangeRetainedReopenAndReclaim(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			var key *[32]byte
			if encrypted {
				key = &[32]byte{7}
			}
			const blocks = 2048
			s, path := newTestStore(t, blocks, key)
			stopTestGC(s)
			active := map[uint64][]byte{}
			var seed []Page
			for b := uint64(0); b < 1536; b++ {
				if b%11 == 0 {
					continue
				}
				data := pageFor(b, 1)
				if b == 512 || b == 513 {
					clear(data)
				}
				seed = append(seed, Page{b, data})
				active[b] = data
			}
			if err := s.WritePages(ctx, seed); err != nil {
				t.Fatal(err)
			}
			payloads := map[uint64]bool{}
			collectPayloads := func() {
				s.mu.Lock()
				defer s.mu.Unlock()
				if err := walkTree(ctx, storeReader{s}, s.ctrl.Root, func(e entry) error {
					payloads[e.slot] = true
					return nil
				}, 0); err != nil {
					t.Fatal(err)
				}
			}
			collectPayloads()
			if err := s.Checkpoint(ctx, "before"); err != nil {
				t.Fatal(err)
			}
			historical := snapshotModel(active)
			for _, b := range []uint64{130, 131, 255, 256, 511, 512, 1023, 1301} {
				data := pageFor(b, 2)
				if err := s.WritePages(ctx, []Page{{b, data}}); err != nil {
					t.Fatal(err)
				}
				active[b] = data
			}
			collectPayloads()
			for _, r := range [][2]uint64{{1, 65}, {253, 70}, {510, 65}, {1200, 64}, {0, 1024}} {
				if err := s.Discard(ctx, int64(r[0])*PageSize, int64(r[1])*PageSize); err != nil {
					t.Fatal(err)
				}
				for b := r[0]; b < r[0]+r[1]; b++ {
					active[b] = nil
				}
				checkActive(t, s, active)
			}
			v, err := s.Export(ctx, "before")
			if err != nil {
				t.Fatal(err)
			}
			checkView(t, v, historical)
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Discard(ctx, 0, blocks*PageSize); err != nil {
				t.Fatal(err)
			}
			for b := uint64(0); b < blocks; b++ {
				active[b] = nil
			}
			checkActive(t, s, active)
			before := s.Stats()
			if err := s.Discard(ctx, 63*PageSize, 130*PageSize); err != nil {
				t.Fatal(err)
			}
			if s.Stats().Commits != before.Commits {
				t.Fatal("repeated Zero discard committed metadata")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path, Options{Key: key})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			stopTestGC(s)
			checkActive(t, s, active)
			v, err = s.Export(ctx, "before")
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			if err := s.DeleteCheckpoint(ctx, "before"); err != nil {
				t.Fatal(err)
			}
			if r := finishTestGC(t, s, 64); r.State != BlockedByReaders {
				t.Fatal("reader did not retain old payload", r)
			}
			checkView(t, v, historical)
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			if r := finishTestGC(t, s, 64); r.State != Complete {
				t.Fatal(r)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.ctrl.Root != 0 {
				t.Fatal("fully discarded tree kept an active root")
			}
			tx := &transaction{store: s, ctrl: s.ctrl}
			free := map[uint64]bool{}
			for slot := s.ctrl.Free; slot != 0; {
				if free[slot] {
					t.Fatal("free-list cycle", slot)
				}
				free[slot] = true
				d, err := tx.desc(slot)
				if err != nil || d.Kind != kindFree {
					t.Fatal("invalid free slot", slot, d, err)
				}
				slot = d.Next
			}
			for slot := range payloads {
				if !free[slot] {
					t.Fatal("removed payload was lost instead of reclaimed", slot)
				}
			}
		})
	}
}

func TestDiscardRangePresencePageBoundary(t *testing.T) {
	const boundary = uint64(presence.PayloadSize * 8)
	s, _ := newTestStore(t, int(boundary+16), nil)
	stopTestGC(s)
	dataBlocks := []uint64{boundary - 4, boundary - 2, boundary + 2, boundary + 5}
	putPages(t, s, dataBlocks, 3)
	if err := s.Checkpoint(ctx, "before"); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(ctx, int64(boundary-3)*PageSize, 8*PageSize); err != nil {
		t.Fatal(err)
	}
	bitmap, err := s.PresenceBitmap(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for b := boundary - 8; b < boundary+12; b++ {
		want := b >= boundary-4 && b <= boundary+5
		if presence.Has(bitmap, b) != want {
			t.Fatalf("presence around encoded page boundary at %d: got %t want %t", b, presence.Has(bitmap, b), want)
		}
	}
	active, err := s.Export(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	old, err := s.Export(ctx, "before")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	for b := boundary - 4; b <= boundary+5; b++ {
		for _, check := range []struct {
			view    *View
			current bool
		}{{active, true}, {old, false}} {
			kind := sparse.Hole
			var expected []byte
			for _, dataBlock := range dataBlocks {
				if b == dataBlock {
					kind, expected = sparse.Data, pageFor(b, 3)
				}
			}
			if check.current && b >= boundary-3 && b < boundary+5 {
				kind, expected = sparse.Zero, nil
			}
			r, err := check.view.RunAt(b*PageSize, PageSize)
			if err != nil || r.Kind() != kind {
				t.Fatalf("block %d current=%t kind=%v want=%v err=%v", b, check.current, r, kind, err)
			}
			data := make([]byte, PageSize)
			if _, err := r.ReadAt(ctx, data, 0); err != nil {
				t.Fatal(err)
			}
			if expected == nil {
				expected = make([]byte, PageSize)
			}
			if !bytes.Equal(data, expected) {
				t.Fatal("presence boundary content mismatch", b, check.current)
			}
		}
	}
}

func TestDiscardRangeCollapsesSeveralRootLevels(t *testing.T) {
	s, _ := newTestStore(t, 128, nil)
	stopTestGC(s)
	putPages(t, s, []uint64{0, 1, 100}, 1)
	// A valid sparse tree may have single-child internal nodes. Wrap this
	// leaf to make the root-collapse case independent of large payload writes.
	s.mu.Lock()
	err := s.mutate(ctx, func(tx *transaction) error {
		for range 2 {
			root, err := tx.allocate(kindIndex)
			if err != nil {
				return err
			}
			tx.nodes[root] = node{entries: []entry{{100, tx.ctrl.Root}}}
			tx.ctrl.Root = root
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "tall"); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(ctx, 0, 2*PageSize); err != nil {
		t.Fatal(err)
	}
	checkActive(t, s, map[uint64][]byte{0: nil, 1: nil, 100: pageFor(100, 1)})
	v, err := s.Export(ctx, "tall")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	checkView(t, v, map[uint64][]byte{0: pageFor(0, 1), 1: pageFor(1, 1), 100: pageFor(100, 1)})
	s.mu.Lock()
	defer s.mu.Unlock()
	n, err := inspectNode(storeReader{s}, s.ctrl.Root)
	if err != nil || !n.leaf || len(n.entries) != 1 {
		t.Fatalf("root did not collapse to remaining leaf: %+v %v", n, err)
	}
}

func TestDiscardRangeCrashHelper(t *testing.T) {
	stage := os.Getenv("DISCARD_RANGE_CRASH_STAGE")
	if stage == "" {
		return
	}
	s, err := Open(os.Getenv("DISCARD_RANGE_CRASH_PATH"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	stopTestGC(s)
	seen := 0
	s.beforeIO = func(actual string) error {
		match := actual == stage
		if stage == "second-apply" && actual == "apply" {
			seen++
			match = seen == 2
		}
		if match {
			os.Exit(87)
		}
		return nil
	}
	if err := s.Discard(ctx, 127*PageSize, 64*PageSize); err != nil {
		t.Fatal(err)
	}
	os.Exit(88)
}

func TestDiscardRangeKilledTransactionsReopen(t *testing.T) {
	for _, stage := range []string{"new-index", "redo-commit", "second-apply", "redo-clear"} {
		t.Run(stage, func(t *testing.T) {
			s, path := newTestStore(t, 512, nil)
			stopTestGC(s)
			want := map[uint64][]byte{}
			var pages []Page
			for b := uint64(0); b < 512; b++ {
				data := pageFor(b, 1)
				pages = append(pages, Page{b, data})
				want[b] = data
			}
			if err := s.WritePages(ctx, pages); err != nil {
				t.Fatal(err)
			}
			if err := s.Checkpoint(ctx, "before"); err != nil {
				t.Fatal(err)
			}
			historical := snapshotModel(want)
			// Isolate the mapping transaction from MarkModified's separate
			// initial invalidation transaction before setting the crash hook.
			if err := s.MarkModified(ctx); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestDiscardRangeCrashHelper$")
			cmd.Env = append(os.Environ(), "DISCARD_RANGE_CRASH_STAGE="+stage, "DISCARD_RANGE_CRASH_PATH="+path)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 87 {
				t.Fatalf("did not stop at %s: %v\n%s", stage, err, out)
			}
			s, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			stopTestGC(s)
			if stage == "second-apply" || stage == "redo-clear" {
				for b := uint64(127); b < 191; b++ {
					want[b] = nil
				}
			}
			checkActive(t, s, want)
			v, err := s.Export(ctx, "before")
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			checkView(t, v, historical)
		})
	}
}
