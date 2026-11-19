package uffd

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

func TestBackendMemoryGuardLegacySelection(t *testing.T) {
	for _, tc := range []struct {
		name          string
		first, legacy error
		wantCalls     int
		wantErr       error
	}{
		{"modern", nil, nil, 1, nil},
		{"legacy", unix.EINVAL, nil, 2, nil},
		{"permission", unix.EPERM, nil, 1, unix.EPERM},
		{"access", unix.EACCES, nil, 1, unix.EACCES},
		{"unavailable", unix.ENOSYS, nil, 1, unix.ENOSYS},
		{"legacy-permission", unix.EINVAL, unix.EPERM, 2, unix.EPERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var flags []int
			fd, err := createBackendGuardUFFD(func(flag int) (int, error) {
				flags = append(flags, flag)
				e := tc.first
				if len(flags) > 1 {
					e = tc.legacy
				}
				if e != nil {
					return -1, e
				}
				return 42, nil
			})
			want := []int{unix.O_CLOEXEC | uffdUserModeOnly}
			if tc.wantCalls == 2 {
				want = append(want, unix.O_CLOEXEC)
			}
			if !reflect.DeepEqual(flags, want) || !errors.Is(err, tc.wantErr) || (err == nil && fd != 42) {
				t.Fatalf("flags=%v fd=%d err=%v", flags, fd, err)
			}
		})
	}
}

func TestBackendMemoryGuardRejectsInvalidInput(t *testing.T) {
	if os.Getpagesize() != PageSize {
		t.Skip("4 KiB UFFD page semantics required")
	}
	for _, tc := range []struct {
		base uintptr
		size int
	}{{0, PageSize}, {1, PageSize}, {PageSize, 0}, {PageSize, 1}, {^uintptr(0) &^ uintptr(PageSize-1), PageSize}} {
		if g, err := NewBackendMemoryGuard(-1, tc.base, tc.size); g != nil || !errors.Is(err, unix.EINVAL) {
			t.Fatalf("range=%+v guard=%v err=%v", tc, g, err)
		}
	}
	if g, err := NewBackendMemoryGuard(-1, PageSize, PageSize); g != nil || !errors.Is(err, unix.EBADF) {
		t.Fatalf("bad fd guard=%v err=%v", g, err)
	}
	var g *BackendMemoryGuard
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func backendMemoryNativeError(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("SANDBOX_UFFD_GUARD_TEST_REQUIRE") != "1" {
		for _, unsupported := range []error{unix.EPERM, unix.EACCES, unix.ENOSYS, unix.EINVAL, unix.EOPNOTSUPP} {
			if errors.Is(err, unsupported) {
				t.Skipf("native backend guard unavailable: %v", err)
			}
		}
	}
	t.Fatal(err)
}

func backendMemoryFDCount(t *testing.T) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(fds)
}

func TestBackendMemoryGuardNativeFailureCleanup(t *testing.T) {
	before := backendMemoryFDCount(t)
	m, err := memory.Create("backend-guard-cleanup", 2*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	var g *BackendMemoryGuard
	t.Cleanup(func() { _ = m.Close(); _ = g.Close() })
	g, err = NewBackendMemoryGuard(m.FD(), m.Addr(), m.Size())
	if err != nil {
		backendMemoryNativeError(t, err)
	}
	withGuard := backendMemoryFDCount(t)
	duplicate, err := NewBackendMemoryGuard(m.FD(), m.Addr(), m.Size())
	if err == nil || duplicate != nil {
		if duplicate != nil {
			_ = duplicate.Close()
		}
		t.Fatal("accepted an already registered VMA")
	}
	if got := backendMemoryFDCount(t); got != withGuard {
		t.Fatalf("failed registration leaked FD: %d -> %d", withGuard, got)
	}
	guardFD := g.fd
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = unix.FcntlInt(uintptr(guardFD), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("guard FD remains open: %v", err)
	}
	if got := backendMemoryFDCount(t); got != before {
		t.Fatalf("cleanup FD count %d -> %d", before, got)
	}

	nonfresh, err := memory.Create("backend-guard-nonfresh", 2*PageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer nonfresh.Close()
	nonfresh.Bytes()[PageSize] = 0x5a
	beforeReject := backendMemoryFDCount(t)
	rejected, err := NewBackendMemoryGuard(nonfresh.FD(), nonfresh.Addr(), nonfresh.Size())
	if rejected != nil || err == nil || !strings.Contains(err.Error(), "already has data") {
		if rejected != nil {
			_ = rejected.Close()
		}
		t.Fatalf("nonfresh guard=%v err=%v", rejected, err)
	}
	if got := backendMemoryFDCount(t); got != beforeReject {
		t.Fatalf("nonfresh rejection leaked FD: %d -> %d", beforeReject, got)
	}
	if nonfresh.Bytes()[PageSize] != 0x5a {
		t.Fatal("nonfresh rejection modified existing data")
	}
}

func TestBackendMemoryGuardNativeReleaseAndReload(t *testing.T) {
	// Register this cleanup before realShmemHandler's cleanups: its handler,
	// mappings and memfd must be gone before the independent guard closes.
	var g *BackendMemoryGuard
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	source := newRecordingSnapshot(PageSize, sparse.Data, 0xa5)
	h, backend, cpu := realShmemHandler(t, 1, source)
	var err error
	g, err = NewBackendMemoryGuard(h.cfg.MemfdFD, uintptr(unsafe.Pointer(&backend[0])), len(backend))
	if err != nil {
		backendMemoryNativeError(t, err)
	}
	if err = h.EnsureLoaded(context.Background(), 0, PageSize); err != nil {
		t.Fatal(err)
	}
	if backend[0] != 0xa5 {
		t.Fatal("initial cross-alias COPY was not visible")
	}
	// Match CH's release order: its VMA discard publishes REMOVE before the
	// file-level hole punch. Punching the inode alone is not that notification.
	if err = unix.Madvise(cpu, unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	waitRemove(t, h)
	if err = unix.Fallocate(h.cfg.MemfdFD, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, PageSize); err != nil {
		t.Fatal(err)
	}
	if h.state.Get(0) != StateReleased {
		t.Fatal("release was not published")
	}
	if err = h.EnsureLoaded(context.Background(), 0, PageSize); err != nil {
		t.Fatal(err)
	}
	for _, b := range backend {
		if b != 0 {
			t.Fatal("Released reload revived historical snapshot data")
		}
	}
	if h.stats.sourceReadCalls.Load() != 1 || h.state.Get(0) != StateLoaded {
		t.Fatalf("unexpected reload state/stats: %v", h.Stats())
	}
}
