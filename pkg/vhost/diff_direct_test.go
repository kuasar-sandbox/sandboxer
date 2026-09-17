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
			checkDiskActiveIO(t, d)
			if d.direct == nil {
				t.Fatal("active diff has no owned workspace")
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
			checkDiskActiveIO(t, cow.diff)
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
	testDiffIOBeforeTemplateSeed(t, t.TempDir(), false)
}

func TestDiffTmpfsPreparedBeforeTemplateSeed(t *testing.T) {
	testDiffIOBeforeTemplateSeed(t, tmpfsTestDir(t), true)
}

func testDiffIOBeforeTemplateSeed(t *testing.T, dir string, tmpfs bool) {
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
			var fs unix.Statfs_t
			if err := unix.Fstatfs(fd, &fs); err != nil {
				return err
			}
			if tmpfs && fs.Type != unix.TMPFS_MAGIC {
				return fmt.Errorf("seed fd is not tmpfs: %x", fs.Type)
			}
			if flags&unix.O_DIRECT == 0 {
				return fmt.Errorf("seed target did not request O_DIRECT")
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
	if tmpfs {
		checkTmpfsActiveIO(t, target)
	}
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

func checkDiskActiveIO(t *testing.T, d *diffFile) {
	t.Helper()
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(d.f.Fd()), &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != unix.EXT4_SUPER_MAGIC && fs.Type != unix.XFS_SUPER_MAGIC {
		t.Fatalf("DIO test needs real disk-backed ext4/XFS: 0x%x", fs.Type)
	}
	flags, err := unix.FcntlInt(d.f.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_DIRECT == 0 {
		t.Fatalf("O_DIRECT flags=%x err=%v", flags, err)
	}
}

func TestDiffDirectErrorDoesNotFallBack(t *testing.T) {
	cow := cachedCOW(t, testCache(t, 1, 1, cacheHooks{}), 2, nil)
	checkDiskActiveIO(t, cow.diff)
	// Offset 1 is invalid on the verified disk DIO path. Do not retry buffered.
	if _, err := cow.diff.bodyIO.WriteAt(cow.diff.direct.write.bytes[:cowBlockSize], 1); !errors.Is(err, unix.EINVAL) {
		t.Fatalf("unaligned physical write: %v", err)
	}
	checkDiskActiveIO(t, cow.diff)
}

func TestDiffDirectAlignmentFromStatx(t *testing.T) {
	reported := func(memory, offset uint32) unix.Statx_t {
		return unix.Statx_t{Mask: unix.STATX_DIOALIGN, Dio_mem_align: memory, Dio_offset_align: offset}
	}
	for _, tc := range []struct {
		name           string
		stat           unix.Statx_t
		queryErr       error
		body, size     int64
		memory, offset int
		wantErr        bool
	}{
		{name: "positive", stat: reported(512, 512), body: 4096, size: 8192, memory: 512, offset: 512},
		{name: "independent address alignment", stat: reported(65536, 512), body: 4096, size: 8192, memory: 65536, offset: 512},
		{name: "missing mask", stat: unix.Statx_t{}, body: 4096, size: 8192, memory: 4096, offset: 4096},
		{name: "ignore unreported fields", stat: unix.Statx_t{Dio_mem_align: 512, Dio_offset_align: 0}, body: 4096, size: 8192, memory: 4096, offset: 4096},
		{name: "both zero", stat: reported(0, 0), body: 4096, size: 8192, memory: 4096, offset: 4096},
		{name: "ENOSYS", queryErr: unix.ENOSYS, body: 4096, size: 8192, memory: 4096, offset: 4096},
		{name: "EINVAL", queryErr: unix.EINVAL, body: 4096, size: 8192, memory: 4096, offset: 4096},
		{name: "EOPNOTSUPP", queryErr: unix.EOPNOTSUPP, body: 4096, size: 8192, memory: 4096, offset: 4096},
		{name: "zero address", stat: reported(0, 512), body: 4096, size: 8192, wantErr: true},
		{name: "zero offset", stat: reported(512, 0), body: 4096, size: 8192, wantErr: true},
		{name: "oversized address", stat: reported(2<<20, 512), body: 4096, size: 8192, wantErr: true},
		{name: "oversized offset", stat: reported(4096, 8192), body: 4096, size: 8192, wantErr: true},
		{name: "nondividing offset", stat: reported(4096, 1000), body: 4096, size: 8192, wantErr: true},
		{name: "misaligned body", stat: reported(512, 512), body: 4097, size: 8192, wantErr: true},
		{name: "misaligned size", stat: reported(512, 512), body: 4096, size: 8193, wantErr: true},
		{name: "invalid conservative body", stat: reported(0, 0), body: 512, size: 8192, wantErr: true},
		{name: "invalid conservative size", body: 4096, size: 8193, wantErr: true},
		{name: "EIO", queryErr: unix.EIO, body: 4096, size: 8192, wantErr: true},
		{name: "EBADF", queryErr: unix.EBADF, body: 4096, size: 8192, wantErr: true},
		{name: "EACCES", queryErr: unix.EACCES, body: 4096, size: 8192, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			memory, offset, err := directAlignmentFromStatx(tc.stat, tc.queryErr, tc.body, tc.size)
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid constraint/query accepted")
				}
				if tc.queryErr != nil && !errors.Is(err, tc.queryErr) {
					t.Fatalf("query error lost: %v", err)
				}
				return
			}
			if err != nil || memory != tc.memory || offset != tc.offset {
				t.Fatalf("alignment %d/%d err=%v", memory, offset, err)
			}
		})
	}
}

func TestDiffDirectAlignmentClosedFD(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	d := &diffFile{f: f, bodyIO: f, logicalSize: cowBlockSize}
	if err := d.enableDirect(); !errors.Is(err, unix.EBADF) {
		t.Fatalf("closed fd: %v", err)
	}
	if d.direct != nil {
		t.Fatal("workspace committed after statx failure")
	}
}
