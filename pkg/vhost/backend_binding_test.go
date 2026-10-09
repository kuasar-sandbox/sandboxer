package vhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

func bindingSetForTest(t *testing.T, specs ...BlockDeviceSpec) *BackendSet {
	t.Helper()
	s, err := NewBackendSet(specs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBackendSetUnboundMetadataAndIO(t *testing.T) {
	for _, ro := range []bool{false, true} {
		s := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096, ReadOnly: ro})
		d := s.DeviceBackends()[0]
		if d.Size() != 4096 || d.ReadOnly() != ro {
			t.Fatal("unbound device lost fixed metadata")
		}
		buf := bytes.Repeat([]byte{0x7a}, 512)
		n, err := d.ReadAt(buf, 0)
		if n != 0 || !errors.Is(err, syscall.EIO) || readretry.IsTerminal(err) {
			t.Fatalf("read: %d, %v", n, err)
		}
		if !bytes.Equal(buf, bytes.Repeat([]byte{0x7a}, 512)) {
			t.Fatal("unbound read fabricated data")
		}
		n, err = d.WriteAt(buf, 0)
		if n != 0 || !errors.Is(err, syscall.EIO) || readretry.IsTerminal(err) {
			t.Fatalf("write: %d, %v", n, err)
		}
		if !errors.Is(d.Flush(), syscall.EIO) || !errors.Is(d.Discard(0, 512), syscall.EIO) {
			t.Fatal("unbound operation reported success")
		}
		b := &minimalProfileBackend{memBackend: &memBackend{data: make([]byte, 4096)}, readOnly: ro}
		if err := s.Bind([]Backend{b}); err != nil {
			t.Fatal(err)
		}
		if d.Size() != 4096 || d.ReadOnly() != ro {
			t.Fatal("binding changed device metadata")
		}
	}
}

func TestBackendSetValidatesEveryCandidate(t *testing.T) {
	for _, size := range []int64{-512, 0, 1, 513} {
		if _, err := NewBackendSet([]BlockDeviceSpec{{Capacity: size}}); err == nil {
			t.Fatalf("accepted capacity %d", size)
		}
	}
	s := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096}, BlockDeviceSpec{Capacity: 4096})
	good := &memBackend{data: make([]byte, 4096)}
	var typedNil *memBackend
	candidates := [][]Backend{
		nil, {good}, {good, nil}, {good, typedNil},
		{good, &memBackend{data: make([]byte, 8192)}},
		{good, &minimalProfileBackend{memBackend: good, readOnly: true}},
	}
	for i, bs := range candidates {
		if err := s.Bind(bs); err == nil {
			t.Fatalf("candidate %d accepted", i)
		}
		if s.binding.Load() != nil {
			t.Fatal("failed candidate partially committed")
		}
		for _, d := range s.DeviceBackends() {
			if _, err := d.ReadAt(make([]byte, 512), 0); !errors.Is(err, ErrBackendUnbound) {
				t.Fatalf("partial binding: %v", err)
			}
		}
	}
	if err := s.Bind([]Backend{good, good}); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind([]Backend{good, good}); !errors.Is(err, ErrBackendSetBound) {
		t.Fatalf("rebind: %v", err)
	}
}

func TestBackendSetCopiesInputSlices(t *testing.T) {
	specs := []BlockDeviceSpec{{Capacity: 4096}}
	s, err := NewBackendSet(specs)
	if err != nil {
		t.Fatal(err)
	}
	specs[0].Capacity = 8192
	devices := s.DeviceBackends()
	devices[0] = nil
	b := &memBackend{data: bytes.Repeat([]byte{0x41}, 4096)}
	backends := []Backend{b}
	if err := s.Bind(backends); err != nil {
		t.Fatal(err)
	}
	backends[0] = &memBackend{data: bytes.Repeat([]byte{0x42}, 4096)}
	d := s.DeviceBackends()[0]
	out := make([]byte, 512)
	if n, err := d.ReadAt(out, 0); n != 512 || err != nil || out[0] != 0x41 || d.Size() != 4096 {
		t.Fatalf("aliased input: n=%d err=%v size=%d", n, err, d.Size())
	}
}

type bindingValidationBackend struct {
	Backend
	entered chan struct{}
	release chan struct{}
}

func (b *bindingValidationBackend) Size() int64 {
	close(b.entered)
	<-b.release
	return b.Backend.Size()
}

func waitBindingSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("binding barrier timed out")
	}
}

func TestBackendSetPublishesAfterAllValidation(t *testing.T) {
	s := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096}, BlockDeviceSpec{Capacity: 4096})
	b := &memBackend{data: make([]byte, 4096)}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	allow := func() { once.Do(func() { close(release) }) }
	defer allow()
	blocked := &bindingValidationBackend{Backend: b, entered: entered, release: release}
	done := make(chan error, 1)
	go func() { done <- s.Bind([]Backend{b, blocked}) }()
	waitBindingSignal(t, entered)
	for _, d := range s.DeviceBackends() {
		if _, err := d.ReadAt(make([]byte, 512), 0); !errors.Is(err, ErrBackendUnbound) {
			t.Fatalf("binding visible during validation: %v", err)
		}
	}
	allow()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, d := range s.DeviceBackends() {
		if _, err := d.ReadAt(make([]byte, 512), 0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackendSetConcurrentCommitHasOneWinner(t *testing.T) {
	s := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096}, BlockDeviceSpec{Capacity: 4096})
	const n = 32
	type result struct {
		id  byte
		err error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(id byte) {
			b := &memBackend{data: bytes.Repeat([]byte{id}, 4096)}
			<-start
			results <- result{id: id, err: s.Bind([]Backend{b, b})}
		}(byte(i + 1))
	}
	close(start)
	successes := 0
	var winner byte
	for i := 0; i < n; i++ {
		r := <-results
		if r.err == nil {
			successes++
			winner = r.id
		} else if !errors.Is(r.err, ErrBackendSetBound) {
			t.Fatal(r.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful commits=%d", successes)
	}
	for _, d := range s.DeviceBackends() {
		out := make([]byte, 512)
		_, err := d.ReadAt(out, 0)
		if err != nil || out[0] != winner {
			t.Fatalf("mixed candidate: data=%d winner=%d err=%v", out[0], winner, err)
		}
	}
}

// Two distinct data descriptors exercise the actual processChain dispatch.
func bindingQueue(backend Backend, kind uint32) (*Server, *virtq, []byte) {
	s := NewServer("unused", backend, nil)
	mem := make([]byte, 4096)
	const uva = uint64(0x1000)
	s.SetMemoryLoader(initializedTestMemory)
	s.memTable.SetRegions([]MemRegion{{GuestPhysAddr: 0, UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
	q := &virtq{num: 8, descAddr: uva, kickFd: -1, callFd: -1, ctx: context.Background(), stop: make(chan struct{})}
	desc := func(i int, addr uint64, n uint32, flags, next uint16) {
		d := mem[i*16 : (i+1)*16]
		binary.LittleEndian.PutUint64(d, addr)
		binary.LittleEndian.PutUint32(d[8:], n)
		binary.LittleEndian.PutUint16(d[12:], flags)
		binary.LittleEndian.PutUint16(d[14:], next)
	}
	binary.LittleEndian.PutUint32(mem[256:], kind)
	desc(0, 256, 16, descFlagNext, 1)
	flags := uint16(descFlagNext)
	if kind == BlkTypeIn {
		flags |= descFlagWrite
	}
	desc(1, 512, 512, flags, 2)
	desc(2, 1024, 512, flags, 3)
	desc(3, 1536, 1, descFlagWrite, 0)
	if kind == BlkTypeFlush {
		desc(0, 256, 16, descFlagNext, 3)
	}
	copy(mem[512:1536], bytes.Repeat([]byte{0xa5}, 1024))
	mem[1536] = 0xff
	return s, q, mem
}

func TestBackendSetUnboundRequestsCompleteIOERR(t *testing.T) {
	for _, kind := range []uint32{BlkTypeIn, BlkTypeOut, BlkTypeFlush} {
		t.Run(BlkReqTypeName(kind), func(t *testing.T) {
			set := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096})
			srv, q, mem := bindingQueue(set.DeviceBackends()[0], kind)
			n, err := srv.processChain(q, 0)
			if err != nil || n != 1 || mem[1536] != BlkStatusIOErr {
				t.Fatalf("unbound request n=%d err=%v status=%d", n, err, mem[1536])
			}
			if !bytes.Equal(mem[512:1536], bytes.Repeat([]byte{0xa5}, 1024)) {
				t.Fatal("unbound request changed guest data")
			}
			real := &memBackend{data: bytes.Repeat([]byte{0x31}, 4096)}
			if err := set.Bind([]Backend{real}); err != nil {
				t.Fatal(err)
			}
			n, err = srv.processChain(q, 0)
			want := 1
			if kind == BlkTypeIn {
				want = 1025
			}
			if err != nil || n != want || mem[1536] != BlkStatusOK {
				t.Fatalf("bound request n=%d err=%v status=%d", n, err, mem[1536])
			}
		})
	}
}

type selectingRequestBackend struct {
	Backend
	selectBackend func() Backend
}

func (b *selectingRequestBackend) requestBackend() Backend { return b.selectBackend() }

func TestBackendSetPinsWholeRequest(t *testing.T) {
	for _, kind := range []uint32{BlkTypeIn, BlkTypeOut, BlkTypeFlush} {
		t.Run(BlkReqTypeName(kind), func(t *testing.T) {
			a := &minimalProfileBackend{memBackend: &memBackend{data: bytes.Repeat([]byte{0x31}, 4096)}}
			b := &minimalProfileBackend{memBackend: &memBackend{data: bytes.Repeat([]byte{0x62}, 4096)}}
			var selections atomic.Int32
			selector := &selectingRequestBackend{Backend: b, selectBackend: func() Backend {
				if selections.Add(1) == 1 {
					return a
				}
				return b
			}}
			srv, q, mem := bindingQueue(selector, kind)
			if _, err := srv.processChain(q, 0); err != nil {
				t.Fatal(err)
			}
			if selections.Load() != 1 || mem[1536] != BlkStatusOK {
				t.Fatalf("selections=%d status=%d", selections.Load(), mem[1536])
			}
			if kind == BlkTypeIn && !bytes.Equal(mem[512:1536], bytes.Repeat([]byte{0x31}, 1024)) {
				t.Fatal("segmented read used different binding views")
			}
			if kind == BlkTypeOut && !bytes.Equal(a.data[:1024], bytes.Repeat([]byte{0xa5}, 1024)) {
				t.Fatal("segmented write used different binding views")
			}
			if !bytes.Equal(b.data, bytes.Repeat([]byte{0x62}, 4096)) || b.flushes != 0 {
				t.Fatal("request reached unselected backend")
			}
			if kind == BlkTypeFlush && a.flushes != 1 {
				t.Fatal("flush did not use selected backend")
			}
		})
	}
}

func TestBackendSetCommitDoesNotRetargetInflightRequest(t *testing.T) {
	set := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096})
	d := set.DeviceBackends()[0]
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	allow := func() { once.Do(func() { close(release) }) }
	defer allow()
	selector := &selectingRequestBackend{Backend: d, selectBackend: func() Backend {
		view := backendForRequest(d)
		close(entered)
		<-release
		return view
	}}
	srv, q, mem := bindingQueue(selector, BlkTypeIn)
	done := make(chan error, 1)
	go func() { _, err := srv.processChain(q, 0); done <- err }()
	waitBindingSignal(t, entered)
	if err := set.Bind([]Backend{&memBackend{data: bytes.Repeat([]byte{0x31}, 4096)}}); err != nil {
		t.Fatal(err)
	}
	allow()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if mem[1536] != BlkStatusIOErr {
		t.Fatalf("inflight request retargeted: status=%d", mem[1536])
	}
	srv.backend = d
	if _, err := srv.processChain(q, 0); err != nil || mem[1536] != BlkStatusOK {
		t.Fatalf("next request failed: %v status=%d", err, mem[1536])
	}
}

type bindingContextKey struct{}
type bindingContextBackend struct {
	*memBackend
	readContext, writeContext bool
}

func (b *bindingContextBackend) readAt(ctx context.Context, p []byte, off int64) (int, error) {
	b.readContext = ctx.Value(bindingContextKey{}) == true
	return b.memBackend.ReadAt(p, off)
}
func (b *bindingContextBackend) writeAt(ctx context.Context, p []byte, off int64) (int, error) {
	b.writeContext = ctx.Value(bindingContextKey{}) == true
	return b.memBackend.WriteAt(p, off)
}

func TestBackendSetPreservesContextDispatch(t *testing.T) {
	set := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096})
	b := &bindingContextBackend{memBackend: &memBackend{data: make([]byte, 4096)}}
	if err := set.Bind([]Backend{b}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []uint32{BlkTypeIn, BlkTypeOut} {
		srv, q, _ := bindingQueue(set.DeviceBackends()[0], kind)
		q.ctx = context.WithValue(context.Background(), bindingContextKey{}, true)
		if _, err := srv.processChain(q, 0); err != nil {
			t.Fatal(err)
		}
	}
	if !b.readContext || !b.writeContext {
		t.Fatal("binding dropped context-aware backend methods")
	}
}

func TestBackendSetBoundCOWReadWrite(t *testing.T) {
	cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), nil, DiffInit{CreateSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cow.Close(); err != nil {
			t.Error(err)
		}
	})
	set := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096})
	if err := set.Bind([]Backend{&CowBackend{C: cow}}); err != nil {
		t.Fatal(err)
	}
	d := set.DeviceBackends()[0]
	srv, q, mem := bindingQueue(d, BlkTypeOut)
	if _, err := srv.processChain(q, 0); err != nil || mem[1536] != BlkStatusOK {
		t.Fatalf("COW write: %v status=%d", err, mem[1536])
	}
	srv, q, mem = bindingQueue(d, BlkTypeIn)
	// Do not initialize the destination with the expected bytes: that would
	// let a success-without-reading regression pass this test.
	copy(mem[512:1536], bytes.Repeat([]byte{0xcc}, 1024))
	if _, err := srv.processChain(q, 0); err != nil || mem[1536] != BlkStatusOK {
		t.Fatalf("COW read: %v status=%d", err, mem[1536])
	}
	if !bytes.Equal(mem[512:1536], bytes.Repeat([]byte{0xa5}, 1024)) {
		t.Fatal("COW data changed after binding")
	}
	if stats := d.(StatsReporter).BackendStats(); stats == nil {
		t.Fatal("bound COW statistics not forwarded")
	}
}

func TestBackendSetRejectsDeviceFrontendBindings(t *testing.T) {
	s := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096})
	other := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096})
	for _, candidate := range []Backend{s.DeviceBackends()[0], other.DeviceBackends()[0]} {
		if err := s.Bind([]Backend{candidate}); err == nil {
			t.Fatal("accepted a device frontend rather than prepared workload storage")
		}
		if s.binding.Load() != nil {
			t.Fatal("invalid frontend candidate was committed")
		}
	}
	if err := s.Bind([]Backend{&memBackend{data: make([]byte, 4096)}}); err != nil {
		t.Fatal(err)
	}
}
