package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/sys/unix"
)

type failingPageStream struct {
	fetch.Stream
	n        int
	cause    error
	metadata bool
	calls    int
}

func (s *failingPageStream) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	s.calls++
	if s.calls == 1 {
		return s.n, s.cause
	}
	return s.Stream.ReadAt(ctx, p, off)
}
func (s *failingPageStream) RunAt(off, limit uint64) (sparse.Run, error) {
	if s.metadata {
		s.calls++
		if s.calls == 1 {
			return nil, s.cause
		}
	}
	return s.Stream.RunAt(off, limit)
}

func TestMixedSnapshotPageAndMetadataEOFClassification(t *testing.T) {
	for _, tt := range []struct {
		name      string
		n         int
		cause     error
		permanent bool
	}{
		{"short EOF", 17, io.EOF, true},
		{"short unexpected EOF", 17, io.ErrUnexpectedEOF, true},
		{"full unexpected EOF", PageSize, io.ErrUnexpectedEOF, true},
		{"full EOF", PageSize, io.EOF, false},
		{"retryable EOF", 17, readerr.Mark(io.EOF, true), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, err := sparse.NewSource(bytes.NewReader(bytes.Repeat([]byte{0x42}, PageSize)), PageSize, []sparse.Extent{{Offset: 0, Size: 17}})
			if err != nil {
				t.Fatal(err)
			}
			stream := &failingPageStream{Stream: testStream{base}, n: tt.n, cause: tt.cause}
			source, _ := NewStreamSnapshotSource(stream, PageSize)
			run, err := source.RunAt(0, PageSize)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := run.(streamPageRun); !ok {
				t.Fatalf("not mixed page fallback: %T", run)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = readretry.ReadAt(ctx, PageSize, func() (int, error) { return run.ReadAt(ctx, make([]byte, PageSize), 0) })
			if tt.permanent {
				if !readerr.IsPermanent(err) || !errors.Is(err, tt.cause) || stream.calls != 1 {
					t.Fatalf("truncation retried: calls=%d err=%v", stream.calls, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, cause := range []error{io.EOF, io.ErrUnexpectedEOF, readerr.Mark(io.EOF, true)} {
		base, _ := sparse.NewSource(bytes.NewReader(make([]byte, PageSize)), PageSize, nil)
		stream := &failingPageStream{Stream: testStream{base}, metadata: true, cause: cause}
		source, _ := NewStreamSnapshotSource(stream, PageSize)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := readretry.Do(ctx, func() error { _, err := source.RunAt(0, PageSize); return err })
		cancel()
		if cause == io.EOF || cause == io.ErrUnexpectedEOF {
			if !readerr.IsPermanent(err) || stream.calls != 1 {
				t.Fatalf("metadata EOF retried: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

type recoveringChunkRun struct {
	fetch.ChunkRun
	attempts         atomic.Int32
	entered, release chan struct{}
}

func (r *recoveringChunkRun) ReadAt(ctx context.Context, p []byte, offset uint64) (int, error) {
	if len(p) != chunkFaultFillBytes || offset != 0 {
		return 0, readerr.Mark(errors.New("required window changed"), false)
	}
	if r.attempts.Add(1) == 1 {
		for i := range p {
			p[i] = 0xff
		}
		return len(p) / 2, io.ErrClosedPipe
	}
	close(r.entered)
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-r.release:
	}
	return r.ChunkRun.ReadAt(ctx, p, offset)
}

type recoveringChunkSource struct {
	SnapshotReader
	window *recoveringChunkRun
}

func (s *recoveringChunkSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	return s.SnapshotReader.RunAt(offset, limit)
}
func (s *recoveringChunkSource) resolveChunkWindow(fetch.ChunkRun, uint64) (fetch.ChunkRun, error) {
	return s.window, nil
}

func TestRequiredChunkWindowRecoveryOwnsTailBuffer(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "cancel"}[stop], func(t *testing.T) {
			plain := bytes.Repeat([]byte{0x6c}, chunkFaultFillBytes)
			key := sha256.Sum256(plain)
			m := &codec.Manifest{Version: codec.Version1, ImageSize: uint64(len(plain)), Entries: []codec.ChunkEntry{{Size: uint32(len(plain)), CiphertextHash: key}}}
			stream, _ := openSnapshotManifest(t, m, map[store.ContentKey][]byte{key: plain})
			run, err := stream.RunAt(0, uint64(len(plain)))
			if err != nil {
				t.Fatal(err)
			}
			window := &recoveringChunkRun{ChunkRun: run.(fetch.ChunkRun), entered: make(chan struct{}), release: make(chan struct{})}
			source, err := NewStreamSnapshotSource(stream, uint64(len(plain)))
			if err != nil {
				t.Fatal(err)
			}
			h := newUnitHandler(t, uint64(len(plain)), &recoveringChunkSource{SnapshotReader: source, window: window})
			ops := newFakeIoctls()
			h.ops = ops.ops()
			startUnitTail(t, h)
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.handleFault(faultEvent{address: unitCHVA, uffdFD: 9}, make([]byte, PageSize))
			}()
			receiveSignal(t, window.entered)
			if !h.tailBusy.Load() || len(ops.snapshot()) != 0 {
				t.Fatal("buffer released or bytes published during retry")
			}
			if stop {
				h.cancel()
			} else {
				close(window.release)
			}
			receiveSignal(t, done)
			waitUnitTail(t, h)
			if h.tailBusy.Load() || window.attempts.Load() != 2 {
				t.Fatal("tail reservation leaked or request replayed")
			}
			calls := ops.snapshot()
			if stop {
				if len(calls) != 0 {
					t.Fatal("canceled fault published data")
				}
				return
			}
			if len(calls) != 2 || calls[0].length != PageSize || !bytes.Equal(calls[0].data, plain[:PageSize]) || !bytes.Equal(calls[1].data, plain[PageSize:]) {
				t.Fatalf("partial failed bytes leaked or duplicate completion: %+v", calls)
			}
		})
	}
}

func TestRequiredFaultRetriesButTailRemainsBestEffort(t *testing.T) {
	source := newRecordingSnapshot(20*PageSize, sparse.Data, 0x42)
	var urgent, tail atomic.Int32
	source.readHook = func(_ context.Context, off uint64, p []byte) error {
		if off != 0 {
			tail.Add(1)
			return io.ErrClosedPipe
		}
		n := urgent.Add(1)
		if n <= 3 {
			for i := range p {
				p[i] = byte(n)
			}
			return []error{io.ErrClosedPipe, context.DeadlineExceeded, context.Canceled}[n-1]
		}
		return nil
	}
	h := newUnitHandler(t, 20*PageSize, source)
	ops := newFakeIoctls()
	h.ops = ops.ops()
	startUnitTail(t, h)
	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 12}, make([]byte, PageSize))
	waitUnitTail(t, h)
	if urgent.Load() != 4 || tail.Load() != 1 || h.state.Get(0) != StateLoaded || h.state.Get(1) != StateAbsent || h.Stats()["errors"] != 0 {
		t.Fatalf("urgent=%d tail=%d states=%v/%v", urgent.Load(), tail.Load(), h.state.Get(0), h.state.Get(1))
	}
}
func TestRequiredFaultPermanentEOFDoesNotInstallOrWake(t *testing.T) {
	source := newRecordingSnapshot(PageSize, sparse.Data, 0x42)
	source.readErr = readerr.Mark(io.EOF, false)
	h := newUnitHandler(t, PageSize, source)
	ops := newFakeIoctls()
	h.ops = ops.ops()
	var fatal error
	h.cfg.ReadFatal = func(err error) { fatal = err }
	h.handleFault(faultEvent{address: unitCHVA, uffdFD: 12}, make([]byte, PageSize))
	if fatal == nil || !errors.Is(fatal, io.EOF) || h.state.Get(0) != StateAbsent || h.stats.copies.Load() != 0 || h.stats.wakes.Load() != 0 {
		t.Fatalf("fatal=%v stats=%v", fatal, h.Stats())
	}
}

// Real kernel COPY/REMOVE exercise the bytes, not only the conditional state
// update. USER_MODE_ONLY keeps this anonymous-mapping test unprivileged.
func TestRealUffdRemoveDuringReadRecovery(t *testing.T) {
	mem, err := unix.Mmap(-1, 0, PageSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mem)
	fd, err := createUffd(unix.O_CLOEXEC | unix.O_NONBLOCK | 1)
	if err != nil {
		t.Fatalf("real userfaultfd required: %v", err)
	}
	defer unix.Close(fd)
	if _, err = ioctlUffdAPI(fd, uffdFeatureEventRemove); err != nil {
		t.Fatal(err)
	}
	va := uint64(uintptr(unsafe.Pointer(&mem[0])))
	if err = ioctlUffdRegister(fd, va, PageSize); err != nil {
		t.Fatal(err)
	}
	source := newRecordingSnapshot(PageSize, sparse.Data, 0x77)
	entered, release := make(chan struct{}), make(chan struct{})
	var attempts atomic.Int32
	source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
		if attempts.Add(1) == 1 {
			return io.ErrClosedPipe
		}
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := newUnitHandlerWithoutMap(t, PageSize, source)
	defer h.cancel()
	if err = h.addrMap.RegisterVMA(ProcessCH, va, PageSize, 0); err != nil {
		t.Fatal(err)
	}
	h.ops = realUffdOps
	done := make(chan struct{})
	go func() { defer close(done); h.handleFault(faultEvent{address: va, uffdFD: fd}, make([]byte, PageSize)) }()
	receiveSignal(t, entered)
	removed := make(chan error, 1)
	go func() { removed <- unix.Madvise(mem, unix.MADV_DONTNEED) }()
	// Read the actual kernel event before allowing the source to recover.
	var event [32]byte
	deadline := time.Now().Add(time.Second)
	for {
		n, err := unix.Read(fd, event[:])
		if n == 32 {
			break
		}
		if err != unix.EAGAIN || time.Now().After(deadline) {
			t.Fatalf("REMOVE event n=%d err=%v", n, err)
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	msg := (*uffdMsg)(unsafe.Pointer(&event[0]))
	if msg.Event != uffdEventRemove {
		t.Fatalf("event=%x", msg.Event)
	}
	remove := (*uffdMsgRemove)(unsafe.Pointer(&msg.Arg[0]))
	h.state.SetRange((remove.Start-va)/PageSize, (remove.End-va)/PageSize, StateReleased)
	close(release)
	receiveSignal(t, done)
	if !bytes.Equal(mem, make([]byte, PageSize)) {
		t.Fatalf("source recovery installed stale bytes after kernel REMOVE: first=%x state=%v", mem[0], h.state.Get(0))
	}
}

func TestFullFaultQueueCancellationJoinsReaderAndWorkers(t *testing.T) {
	var fds [2]int
	if err := unix.Pipe2(fds[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[1])
	source := newRecordingSnapshot(PageSize, sparse.Data, 0x42)
	entered := make(chan struct{}, 1)
	source.readHook = func(ctx context.Context, _ uint64, _ []byte) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	mapping := NewAddressMap(PageSize)
	if err := mapping.RegisterVMA(ProcessCH, unitCHVA, PageSize, 0); err != nil {
		t.Fatal(err)
	}
	memfd, err := unix.Open("/dev/null", unix.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(memfd)
	h, err := NewWithBackendUffd(fds[0], mapping, Config{MemfdFD: memfd, Size: PageSize, Source: source, NumWorkers: MinWorkers})
	if err != nil {
		t.Fatal(err)
	}
	h.Start()
	defer h.Close()
	written := make(chan struct{})
	writerCtx, cancelWriter := context.WithCancel(context.Background())
	defer cancelWriter()
	go func() {
		defer close(written)
		var raw [32]byte
		raw[0] = uffdEventPagefault
		*(*uint64)(unsafe.Pointer(&raw[16])) = unitCHVA
		for i := 0; i < 300; {
			if _, err := unix.Write(fds[1], raw[:]); err == nil {
				i++
				continue
			}
			select {
			case <-writerCtx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	receiveSignal(t, entered)
	deadline := time.Now().Add(time.Second)
	for h.QueueDepth() < 256 {
		if time.Now().After(deadline) {
			t.Fatalf("queue never filled: %d", h.QueueDepth())
		}
		time.Sleep(time.Millisecond)
	}
	cancelWriter()
	receiveSignal(t, written)
	done := make(chan error, 1)
	go func() { done <- h.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full fault queue deadlocked Close")
	}
	select {
	case <-h.readerDone:
	default:
		t.Fatal("Close returned before reader join")
	}
	if h.stats.inflight.Load() != 0 || h.tailBusy.Load() {
		t.Fatal("Close retained source work")
	}
}
