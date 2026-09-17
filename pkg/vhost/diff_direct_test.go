package vhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestDiffDirectAlignmentAndUnalignedCallers(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			var opts []BlockCOWOption
			if encrypted {
				opts = append(opts, WithDiffEncryption(testDiffKey(8), true))
			}
			cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), nil, DiffInit{CreateSize: 3 * maxDiffScratchSize}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			d := cow.diff
			if d.direct == nil {
				t.Fatal("active diff is buffered")
			}
			flags, err := unix.FcntlInt(d.f.Fd(), unix.F_GETFL, 0)
			if err != nil || flags&unix.O_DIRECT == 0 {
				t.Fatalf("O_DIRECT flags=%x err=%v", flags, err)
			}
			for _, m := range []*alignedMapping{d.direct.read, d.direct.write} {
				if uintptr(unsafe.Pointer(&m.bytes[0]))%uintptr(d.direct.memoryAlign) != 0 {
					t.Fatal("unaligned mmap")
				}
				if len(m.bytes) != maxDiffScratchSize {
					t.Fatal("unbounded DIO buffer")
				}
			}
			// Arbitrary heap sub-slices cross both 4 KiB and the bounded 1 MiB window.
			backing := patternedBytes(2*maxDiffScratchSize + 2)
			payload := backing[1 : len(backing)-1]
			if _, err := d.WriteAt(payload, 0); err != nil {
				t.Fatal(err)
			}
			update := []byte("subsector update")
			if _, err := d.WriteAt(update, 513); err != nil {
				t.Fatal(err)
			}
			copy(payload[513:], update)
			got := make([]byte, len(payload)+2)
			if n, err := d.ReadAt(got[1:len(got)-1], 0); err != nil || n != len(payload) {
				t.Fatalf("unaligned read n=%d %v", n, err)
			}
			if !bytes.Equal(got[1:len(got)-1], payload) {
				t.Fatal("DIO/XTS lost bytes")
			}
			if !allZero(d.direct.read.bytes) || !allZero(d.direct.write.bytes) {
				t.Fatal("plaintext/cipher scratch was not cleared")
			}
			if n, err := d.ReadAt(got[:17], d.logicalSize-8); n != 8 || !errors.Is(err, io.EOF) {
				t.Fatalf("EOF n=%d %v", n, err)
			}
		})
	}
	for _, alignment := range [][2]int{{0, 512}, {512, 0}, {4096, 8192}, {4096, 1000}, {2 << 20, 512}} {
		if err := validateDirectAlignment(alignment[0], alignment[1], 4096, 8192); err == nil {
			t.Fatalf("invalid DIO alignment accepted %v", alignment)
		}
	}
	// Address alignment M is independent of offset/length alignment A.
	if err := validateDirectAlignment(65536, 512, 4096, 8192); err != nil {
		t.Fatal(err)
	}
}

func TestDiffDirectRejectsTmpfsWithoutFallback(t *testing.T) {
	dir, err := os.MkdirTemp("/dev/shm", "diff-cow-230-test-")
	if err != nil {
		t.Fatalf("tmpfs negative-test fixture: %v", err)
	}
	defer os.RemoveAll(dir)
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.TMPFS_MAGIC {
		t.Fatalf("negative-test fixture is not tmpfs: 0x%x", fs.Type)
	}
	path := filepath.Join(dir, "active.diff")
	if c, err := OpenBlockCOW(path, nil, DiffInit{CreateSize: cowBlockSize}); err == nil {
		_ = c.Close()
		t.Fatal("tmpfs used as real DIO")
	} else if !strings.Contains(err.Error(), "unsupported active diff filesystem") {
		t.Fatalf("missing diagnostic %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unsupported target committed: %v", err)
	}
	// The same filesystem remains valid for an immutable buffered template.
	if err := os.WriteFile(path, make([]byte, cowBlockSize), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := openDiffTemplate(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.ReadAt(context.Background(), make([]byte, 17), 0); err != nil {
		t.Fatal(err)
	}
}

// mincore only queries residency; it never faults data in or evicts/flushes it.
func diffResidentPages(t *testing.T, d *diffFile) int {
	t.Helper()
	mapping, err := unix.Mmap(int(d.f.Fd()), d.bodyOffset, int(d.logicalSize), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mapping)
	vec := make([]byte, (len(mapping)+os.Getpagesize()-1)/os.Getpagesize())
	_, _, errno := unix.Syscall(unix.SYS_MINCORE, uintptr(unsafe.Pointer(&mapping[0])), uintptr(len(mapping)), uintptr(unsafe.Pointer(&vec[0])))
	if errno != 0 {
		t.Fatal(errno)
	}
	count := 0
	for _, v := range vec {
		if v&1 != 0 {
			count++
		}
	}
	return count
}
func TestDiffDirectBodyDoesNotPopulatePageCache(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			cache := testCache(t, 8, 4, cacheHooks{})
			var opts []BlockCOWOption
			if encrypted {
				opts = append(opts, WithDiffEncryption(testDiffKey(9), true))
			}
			cow := cachedCOW(t, cache, 1024, nil, opts...)
			payload := patternedBytes(maxDiffScratchSize)
			for off := int64(0); off < cow.size; off += int64(len(payload)) {
				if _, err := cow.WriteAt(payload, off); err != nil {
					t.Fatal(err)
				}
			}
			if err := cow.Drain(context.Background()); err != nil {
				t.Fatal(err)
			}
			for off := int64(0); off < cow.size; off += int64(len(payload)) {
				if _, err := cow.ReadAt(payload, off); err != nil {
					t.Fatal(err)
				}
			}
			if resident := diffResidentPages(t, cow.diff); resident != 0 {
				t.Fatalf("active DIO body has %d resident file-cache pages", resident)
			}
			s := cache.Stats()
			if s.PeakUsed > s.Capacity || s.PeakDirty > s.MaxDirty {
				t.Fatalf("budget %+v", s)
			}
		})
	}
}

type directSeedProbe struct {
	diffTemplateSource
	check func() error
}

func (p directSeedProbe) ReadAt(ctx context.Context, buf []byte, off uint64) (int, error) {
	if err := p.check(); err != nil {
		return 0, err
	}
	return p.diffTemplateSource.ReadAt(ctx, buf, off)
}
func TestDiffDirectEnabledBeforeTemplateSeed(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "template")
	if err := os.WriteFile(srcPath, patternedBytes(2*cowBlockSize), 0600); err != nil {
		t.Fatal(err)
	}
	src, err := openDiffTemplate(srcPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.(*fileDiffTemplate).diff.direct != nil {
		t.Fatal("immutable template unexpectedly direct")
	}
	probeDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	probe := directSeedProbe{src, func() error {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name, err := os.Readlink("/proc/self/fd/" + entry.Name())
			if err != nil {
				continue
			}
			if filepath.Dir(name) != probeDir || !strings.HasPrefix(filepath.Base(name), ".target.") {
				continue
			}
			fd, err := strconv.Atoi(entry.Name())
			if err != nil {
				return err
			}
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
			if err != nil {
				return err
			}
			if flags&unix.O_DIRECT == 0 {
				return fmt.Errorf("target was buffered during seeding")
			}
			calls++
			return nil
		}
		return fmt.Errorf("could not find opened seed target")
	}}
	target, err := initializeFreshDiffFile(filepath.Join(dir, "target"), 2*cowBlockSize, nil, probe)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if calls != 2 {
		t.Fatalf("seed calls=%d", calls)
	}
	got := make([]byte, 2*cowBlockSize)
	if _, err := target.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, patternedBytes(len(got))) {
		t.Fatal("seed content changed")
	}
}

func TestLegacyExt4RejectsSilentBufferedModes(t *testing.T) {
	for _, mode := range []string{"journal", "", "unknown"} {
		if err := validateLegacyExt4(0, mode); err == nil {
			t.Fatalf("accepted data=%s", mode)
		}
	}
	for _, flag := range []int{0x4, 0x800, 0x4000, 0x100000, 0x10000000} {
		if err := validateLegacyExt4(flag, "ordered"); err == nil {
			t.Fatalf("accepted flag %x", flag)
		}
	}
	for _, mode := range []string{"ordered", "writeback"} {
		if err := validateLegacyExt4(0x80000, mode); err != nil {
			t.Fatal(err)
		}
	}
}
