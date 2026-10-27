package diffstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
)

func reopenPresenceCOW(t *testing.T, encrypted bool, extra ...DiskOption) (*Disk, []byte) {
	t.Helper()
	const size = 520 * cowBlockSize
	base := &fakeReader{data: bytes.Repeat([]byte{0x61}, size)}
	path := filepath.Join(t.TempDir(), "active.diff")
	cow := checkpointCOW(t, path, base, size, encrypted)
	want := append([]byte(nil), base.data...)
	for _, block := range []int{0, 2, 255, 256, 519} {
		page := bytes.Repeat([]byte{byte(block)}, cowBlockSize)
		if _, err := cow.WriteAt(page, int64(block*cowBlockSize)); err != nil {
			t.Fatal(err)
		}
		copy(want[block*cowBlockSize:], page)
	}
	if err := cow.Checkpoint(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	if err := cow.Close(); err != nil {
		t.Fatal(err)
	}
	opts := []DiskOption{WithCheckpoint("disk-under-test", "immutable-base")}
	if encrypted {
		opts = append(opts, WithDiffEncryption([32]byte{1, 2, 3}, true))
	}
	opts = append(opts, extra...)
	cow, err := OpenDisk(path, base, DiffInit{Existing: true, CreateSize: size}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cow.Close() })
	return cow, want
}

func TestCheckpointBitmapPresenceReadModifyExport(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			cow, want := reopenPresenceCOW(t, encrypted)
			if stats := cow.BackendStats(); stats["diff_presence_known_blocks"] != 520 || stats["diff_presence_complete"] != true {
				t.Fatal("incomplete bitmap", stats)
			}
			// Registering another retained root keeps the already-complete bitmap.
			if err := cow.Checkpoint(context.Background(), "unscanned"); err != nil {
				t.Fatal(err)
			}
			if cow.BackendStats()["diff_presence_known_blocks"] != 520 {
				t.Fatal("checkpoint lost presence")
			}
			// Partial writes preserve allocated-zero, upper and base content.
			for _, block := range []int{0, 1, 2} {
				off := block*cowBlockSize + 37
				if _, err := cow.WriteAt([]byte{9, 8, 7}, int64(off)); err != nil {
					t.Fatal(err)
				}
				copy(want[off:], []byte{9, 8, 7})
			}
			// Discard an upper block; export must preserve its explicit Zero.
			if err := cow.Discard(255*cowBlockSize, cowBlockSize); err != nil {
				t.Fatal(err)
			}
			clear(want[255*cowBlockSize : 256*cowBlockSize])
			// Snapshot must preserve the untouched tail and exact holes.
			view, err := cow.Export(context.Background(), ActiveDiff)
			if err != nil {
				t.Fatal(err)
			}
			defer view.Close()
			upper := readExport(t, view)
			for block := 0; block < 520; block++ {
				present := block == 0 || block == 1 || block == 2 || block == 255 || block == 256 || block == 519
				page := upper[block*cowBlockSize : (block+1)*cowBlockSize]
				if present {
					if !bytes.Equal(page, want[block*cowBlockSize:(block+1)*cowBlockSize]) {
						t.Fatalf("export block %d", block)
					}
				} else if !bytes.Equal(page, make([]byte, cowBlockSize)) {
					t.Fatalf("export hole %d contains base", block)
				}
			}
			if stats := cow.BackendStats(); stats["diff_presence_complete"] != true || stats["diff_dirty_blocks"] != 6 {
				t.Fatal(stats)
			}
			got := make([]byte, len(want))
			if _, err := cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
				t.Fatal("live view differs", err)
			}
		})
	}
}

func TestCheckpointBitmapPresenceAcceptedWriteWins(t *testing.T) {
	gate := make(chan struct{})
	var release sync.Once
	cache, err := newCache(8*cowBlockSize, 4*cowBlockSize, cacheHooks{beforeWrite: func([]*cachePage) error { <-gate; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cow, want := reopenPresenceCOW(t, false, WithCache(cache))
	defer cow.Close()
	defer release.Do(func() { close(gate) })

	page := bytes.Repeat([]byte{0xc7}, cowBlockSize)
	if _, err := cow.WriteAt(page, cowBlockSize); err != nil {
		t.Fatal(err)
	}

	copy(want[cowBlockSize:], page)
	// The persisted bitmap still says Hole for this accepted cached write.
	// Neither snapshot nor live reads may replace the frontend state with it.
	got := make([]byte, 3*cowBlockSize)
	if _, err := cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, want[:len(got)]) {
		t.Fatal("persisted bitmap hid accepted cache write", err)
	}
	view, _, err := cow.SnapshotView()
	if err != nil {
		t.Fatal(err)
	}
	upper, err := io.ReadAll(view)
	if err != nil || !bytes.Equal(upper[cowBlockSize:2*cowBlockSize], page) || !bytes.Equal(upper[519*cowBlockSize:], want[519*cowBlockSize:]) {
		t.Fatal("snapshot hid accepted write or untouched tail", err)
	}
}

func TestCheckpointBitmapPresenceMutationMetadataOnly(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, operation := range []string{"full-write", "discard"} {
			t.Run(fmt.Sprintf("encrypted=%t/%s", encrypted, operation), func(t *testing.T) {
				cow, _ := reopenPresenceCOW(t, encrypted)
				base := &presenceCountedBase{fakeReader: *cow.base.(*fakeReader)}
				cow.base = base
				backend, err := cow.checkpointBackend()
				if err != nil {
					t.Fatal(err)
				}
				before := backend.store.Stats().DataRead

				page := bytes.Repeat([]byte{0xc7}, cowBlockSize)
				// Cover Hole and Data, then repeat; metadata checks must never read
				// old payload or the lower layer for a complete overwrite/discard.
				for _, block := range []int64{1, 2} {
					for repeat := 0; repeat < 2; repeat++ {
						if operation == "full-write" {
							_, err = cow.WriteAt(page, block*cowBlockSize)
						} else {
							err = cow.Discard(block*cowBlockSize, cowBlockSize)
						}
						if err != nil {
							t.Fatal(err)
						}

					}
				}
				if err := cow.Drain(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := backend.store.Stats().DataRead; got != before || base.reads.Load() != 0 {
					t.Fatalf("%s read old payload: upper bytes=%d base reads=%d", operation, got-before, base.reads.Load())
				}
				want := make([]byte, 2*cowBlockSize)
				if operation == "full-write" {
					want = bytes.Repeat([]byte{0xc7}, 2*cowBlockSize)
				}
				got := make([]byte, len(want))
				if _, err := cow.ReadAt(got, cowBlockSize); err != nil || !bytes.Equal(got, want) {
					t.Fatal("mutation result differs", err)
				}
			})
		}
	}
}

// Mixed loading/writeback/discard serialization is exercised by cow_cache_test.
// The old lazy-query cancellation cases no longer apply: P is loaded at open.
func TestCheckpointBitmapPresenceCanceledExport(t *testing.T) {
	cow, _ := reopenPresenceCOW(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if view, err := cow.activeView(ctx); view != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(view, err)
	}
	if cow.Err() != nil || cow.DirtyCount() != 5 || cow.BackendStats()["diff_presence_complete"] != true {
		t.Fatal("cancellation changed presence", cow.Err(), cow.BackendStats())
	}
	if _, _, err := cow.SnapshotView(); err != nil {
		t.Fatal(err)
	}
}
