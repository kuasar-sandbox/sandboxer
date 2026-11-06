package versioned

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Run only on a fresh finite ext4 owned by the test runner, using the same
// explicit opt-in as TestRealFilesystemFullRecovery. The observer hook never
// returns a fault: both filler exhaustion and mutation pressure come from the
// kernel's actual allocation decisions on this filesystem.
func TestRealFilesystemCacheSpaceRecovery(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached=%t", cached), func(t *testing.T) {
			runRealFilesystemCacheSpace(t, cached)
		})
	}
}

func runRealFilesystemCacheSpace(t *testing.T, cached bool) {
	dir := os.Getenv("DIFFSTORE_FULL_FS")
	if dir == "" {
		t.Skip("requires isolated finite ext4 filesystem")
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	capacity := uint64(fs.Blocks) * uint64(fs.Bsize)
	if fs.Type != unix.EXT4_SUPER_MAGIC || capacity < 32<<20 || capacity > 256<<20 {
		t.Fatalf("requires owned 32–256 MiB ext4, got type=%x capacity=%d", fs.Type, capacity)
	}
	path := filepath.Join(dir, "cache-space-full.diff")
	fillerPath := filepath.Join(dir, "cache-space-fill")
	for _, name := range []string{path, path + ".lock", fillerPath} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owned test path must not already exist: %s (%v)", name, err)
		}
	}
	s, err := Create(path, Options{DiffID: "cache-full", LogicalSize: 2048 * PageSize})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		s.Close()
		os.Remove(path)
		os.Remove(path + ".lock")
	}()
	stopTestGC(s)
	putPages(t, s, []uint64{1000}, 1)
	if err := s.Checkpoint(ctx, "retained"); err != nil {
		t.Fatal(err)
	}
	const oldCount = 128
	const dataCount = MaxBatchPages + 1
	var old []uint64
	var pages []Page
	want := map[uint64][]byte{1000: pageFor(1000, 1)}
	for i := uint64(0); i < oldCount; i++ {
		old = append(old, i+1)
		want[i+1] = nil
	}
	putPages(t, s, old, 2)
	for i := uint64(0); i < dataCount; i++ {
		pages = append(pages, Page{512 + i, pageFor(512+i, 3)})
		want[512+i] = pageFor(512+i, 3)
	}
	if s.ctrl.Garbage != 0 || s.ctrl.Free != 0 {
		t.Fatal("fixture has preexisting reclaimable space", s.ctrl.Garbage, s.ctrl.Free)
	}
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { filler.Close(); os.Remove(fillerPath) }()
	var filled int64
	for _, step := range []int64{1 << 20, 64 << 10, PageSize} {
		for {
			err := unix.Fallocate(int(filler.Fd()), 0, filled, step)
			if errors.Is(err, unix.ENOSPC) {
				break
			}
			if err != nil {
				t.Fatal("fill isolated filesystem", err)
			}
			filled += step
			if uint64(filled) > capacity {
				t.Fatal("filesystem filler exceeded the verified capacity")
			}
		}
	}
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	t.Logf("kernel filler ENOSPC: allocated %d bytes; filesystem available %d bytes", filled, uint64(fs.Bavail)*uint64(fs.Bsize))
	before := s.Stats()
	reserveCalls, zeroBeforePunch := 0, false
	s.beforeIO = func(stage string) error {
		if stage == "reserve" {
			reserveCalls++
		}
		if stage == "gc-punch" {
			// This observer runs under Store.mu. GC must see a committed Zero,
			// with no remaining active payload mapping for the reclaimed source.
			slot, err := lookup(storeReader{s}, s.ctrl.Root, old[0])
			if err != nil {
				return err
			}
			zeroBeforePunch = zeroBeforePunch || slot == 0
		}
		return nil
	}
	write := s.WritePages
	if cached {
		write = s.WritePagesForCache
	}
	err = write(ctx, pages)
	if !errors.Is(err, unix.ENOSPC) || IsSpacePressure(err) != cached {
		t.Fatal("wrong actual ENOSPC classification", cached, err, s.Err())
	}
	if !cached {
		if !errors.Is(s.Err(), unix.ENOSPC) {
			t.Fatal("ordinary Data API lost sticky exhaustion", s.Err())
		}
		s.beforeIO = nil
		_ = s.Close()
		s, err = Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		stopTestGC(s)
		original := map[uint64][]byte{1000: pageFor(1000, 1)}
		for _, block := range old {
			original[block] = pageFor(block, 2)
		}
		checkActive(t, s, original)
		v, err := s.Export(ctx, "retained")
		if err != nil {
			t.Fatal(err)
		}
		checkView(t, v, map[uint64][]byte{1000: pageFor(1000, 1)})
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
		t.Log("REAL_EXT4_ORDINARY_WRITE_STICKY: active and retained content survived kernel ENOSPC")
		return
	}
	deferred := s.Stats()
	if s.Err() != nil || deferred.DataWritten != before.DataWritten || deferred.RedoWritten != before.RedoWritten || deferred.Commits != before.Commits {
		t.Fatal("deferred kernel pressure published failed Data/redo or poisoned Store", err, s.Err(), before, deferred)
	}
	// The cache can now commit its already accepted Zero cohort independently,
	// while retaining the complete frozen Data window for one bounded retry.
	if err := s.DiscardPages(ctx, old); err != nil {
		t.Fatal("Zero cohort could not commit at full filesystem", err)
	}
	if err := s.WritePagesForCache(ctx, pages); err != nil {
		t.Fatal("actual ENOSPC did not recover after the accepted Zero cohort", err)
	}
	s.beforeIO = nil
	after := s.Stats()
	if reserveCalls < 3 || !zeroBeforePunch || after.GCFreed-before.GCFreed < oldCount || after.Reused-before.Reused < dataCount || s.Err() != nil {
		t.Fatal("did not exercise kernel reservation failure, published Zero, punch and reuse", reserveCalls, zeroBeforePunch, before, after, s.Err())
	}
	checkActive(t, s, want)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	stopTestGC(s)
	checkActive(t, s, want)
	v, err := s.Export(ctx, "retained")
	if err != nil {
		t.Fatal(err)
	}
	checkView(t, v, map[uint64][]byte{1000: pageFor(1000, 1)})
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	t.Logf("REAL_EXT4_CACHE_SPACE_RECOVERED: reserve_calls=%d reclaimed=%d reused=%d available=%d", reserveCalls, after.GCFreed-before.GCFreed, after.Reused-before.Reused, uint64(fs.Bavail)*uint64(fs.Bsize))
}
