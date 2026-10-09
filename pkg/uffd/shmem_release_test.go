package uffd

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

func realShmemHandler(t *testing.T, pages int, source SnapshotReader) (*Handler, []byte, []byte) {
	t.Helper()
	size := pages * PageSize
	memfd, err := unix.MemfdCreate("restore-release", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(memfd) })
	if err := unix.Ftruncate(memfd, int64(size)); err != nil {
		t.Fatal(err)
	}
	mmap := func() []byte {
		b, err := unix.Mmap(memfd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unix.Munmap(b) })
		return b
	}
	backend, cpu := mmap(), mmap()
	fd, err := createUffd(unix.O_CLOEXEC | unix.O_NONBLOCK | 1)
	if err == unix.EINVAL {
		fd, err = createUffd(unix.O_CLOEXEC | unix.O_NONBLOCK)
	}
	if err != nil {
		t.Fatalf("real MISSING_SHMEM UFFD required: %v", err)
	}
	owned := false
	defer func() {
		if !owned {
			unix.Close(fd)
		}
	}()
	features := uffdFeatureMissingShmem | uffdFeatureEventRemove | uffdFeatureEventUnmap
	if got, err := ioctlUffdAPI(fd, features); err != nil || got&features != features {
		t.Fatalf("UFFD features=%x err=%v", got, err)
	}
	va := uint64(uintptr(unsafe.Pointer(&cpu[0])))
	if err := ioctlUffdRegister(fd, va, uint64(size)); err != nil {
		t.Fatal(err)
	}
	m := NewAddressMap(uint64(size))
	if err := m.RegisterVMA(ProcessCH, va, uint64(size), 0); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterVMA(ProcessBackend, uint64(uintptr(unsafe.Pointer(&backend[0]))), uint64(size), 0); err != nil {
		t.Fatal(err)
	}
	h, err := NewWithBackendUffd(fd, m, Config{MemfdFD: memfd, Size: size, Source: source, NumWorkers: 2, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	owned = true
	h.Start()
	t.Cleanup(func() { h.Close() })
	return h, backend, cpu
}

func waitRemove(t *testing.T, h *Handler) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.stats.removeEvents.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("real REMOVE was not published")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRealMissingShmemReleaseDuringSourceRead(t *testing.T) {
	for _, cpuFault := range []bool{false, true} {
		t.Run(fmt.Sprintf("cpu_fault_%t", cpuFault), func(t *testing.T) {
			source := newRecordingSnapshot(PageSize, sparse.Data, 0x77)
			entered, resume := make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
				if reads.Add(1) == 1 {
					close(entered)
				}
				select {
				case <-resume:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			h, backend, cpu := realShmemHandler(t, 1, source)
			done := make(chan error, 1)
			go func() {
				if cpuFault {
					if value := cpu[0]; value != 0 {
						done <- fmt.Errorf("stale CPU byte %x", value)
					} else {
						done <- nil
					}
				} else {
					done <- h.EnsureLoaded(context.Background(), 0, PageSize)
				}
			}()
			receiveSignal(t, entered)
			// This real event wakes the CH-side discard thread on read. Installation
			// must remain excluded until Released has been published by the reader.
			if err := unix.Madvise(cpu, unix.MADV_DONTNEED); err != nil {
				t.Fatal(err)
			}
			waitRemove(t, h)
			close(resume)
			if err := loadResult(t, done); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(backend, make([]byte, PageSize)) || !bytes.Equal(cpu, backend) {
				t.Fatal("stale snapshot bytes installed after real REMOVE")
			}
			if reads.Load() != 1 {
				t.Fatalf("Released page reread source %d times", reads.Load())
			}
		})
	}
}

func TestRealMissingShmemReleaseDiscardsSpeculativeTail(t *testing.T) {
	const pages = 4
	source := newRecordingSnapshot(pages*PageSize, sparse.Data, 0x66)
	entered, resume := make(chan struct{}), make(chan struct{})
	source.readHook = func(ctx context.Context, off uint64, _ []byte) error {
		if off == PageSize {
			close(entered)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	h, backend, cpu := realShmemHandler(t, pages, source)
	if cpu[0] != 0x66 {
		t.Fatal("urgent CPU fault did not load snapshot")
	}
	receiveSignal(t, entered)
	if err := unix.Madvise(cpu[PageSize:], unix.MADV_DONTNEED); err != nil {
		t.Fatal(err)
	}
	waitRemove(t, h)
	close(resume)
	waitUnitTail(t, h)
	if err := h.EnsureLoaded(context.Background(), PageSize, (pages-1)*PageSize); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backend[:PageSize], bytes.Repeat([]byte{0x66}, PageSize)) || !bytes.Equal(backend[PageSize:], make([]byte, (pages-1)*PageSize)) {
		t.Fatal("speculative tail revived released snapshot bytes")
	}
	if h.stats.sourceReadCalls.Load() != 2 {
		t.Fatalf("source reads=%d, want urgent plus discarded tail", h.stats.sourceReadCalls.Load())
	}
}

func TestRealMissingShmemEexistPreservesLiveCPUBytes(t *testing.T) {
	source := newRecordingSnapshot(PageSize, sparse.Data, 0xa5)
	h, backend, cpu := realShmemHandler(t, 1, source)
	va, fd, _, err := h.addrMap.registration(0, PageSize)
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Repeat([]byte{0xa5}, PageSize)
	if n, err := realUffdOps.copy(fd, va, original); err != nil || n != PageSize {
		t.Fatalf("competing CH COPY: n=%d err=%v", n, err)
	}
	// Model a winning kernel installation whose state publication has not yet
	// been observed by the mandatory requester; EEXIST must preserve live bytes.
	cpu[512] = 0x3c
	if err := h.EnsureLoaded(context.Background(), 512, 1); err != nil {
		t.Fatal(err)
	}
	original[512] = 0x3c
	if !bytes.Equal(backend, original) {
		t.Fatal("EEXIST overwrote CPU-written folio")
	}
	if h.stats.wakes.Load() != 1 || h.stats.sourceReadCalls.Load() != 1 || h.state.Get(0) != StateLoaded {
		t.Fatalf("unexpected convergence: %v", h.Stats())
	}
}
