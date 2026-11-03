package vhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"syscall"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
	"golang.org/x/sys/unix"
)

const sentinel = 0xa5
const readSector = 7

type windowCall struct {
	offset int64
	length int
}

type windowBackend struct {
	*memBackend
	calls []windowCall
	fault func([]byte, int64) (int, error)
}

func (b *windowBackend) ReadAt(buf []byte, off int64) (int, error) {
	b.calls = append(b.calls, windowCall{off, len(buf)})
	if b.fault != nil {
		return b.fault(buf, off)
	}
	return b.memBackend.ReadAt(buf, off)
}

type windowFixture struct {
	server       *Server
	queue        *virtq
	backend      *windowBackend
	segs         [][]byte
	guards       []*byte
	header, used []byte
	status       *byte
}

func newWindowFixture(t *testing.T, sizes []int, shared bool) *windowFixture {
	t.Helper()
	const num = 128
	const uva = 0x4000
	const avail = num * 16
	const used = avail + 512
	const header = used + 2048
	total := 0
	for _, size := range sizes {
		total += size
	}
	disk := make([]byte, readSector*SectorSize+total)
	for i := range disk {
		disk[i] = byte(i*31 ^ i>>8 ^ i>>16)
	}
	b := &windowBackend{memBackend: &memBackend{data: disk}}
	s := NewServer("", b, nil)
	s.EnableStats("data", "test")
	mem := make([]byte, header+64+total+4*len(sizes))
	s.SetMemoryLoader(initializedTestMemory)
	s.memTable.SetRegions([]MemRegion{{UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q := &virtq{num: num, descAddr: uva, availAddr: uva + avail, usedAddr: uva + used, ctx: ctx, cancel: cancel, stop: make(chan struct{})}
	var err error
	q.callFd, err = unix.Eventfd(0, unix.EFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(q.callFd) })
	f := &windowFixture{server: s, queue: q, backend: b, segs: make([][]byte, len(sizes)), used: mem[used : used+12], header: mem[header : header+16]}
	le := binary.LittleEndian
	put := func(idx, pos, size int, flags uint16) {
		desc := mem[idx*16 : idx*16+16]
		le.PutUint64(desc, uint64(pos))
		le.PutUint32(desc[8:], uint32(size))
		le.PutUint16(desc[12:], flags)
		le.PutUint16(desc[14:], uint16(idx+1))
	}
	put(0, header, 16, descFlagNext)
	le.PutUint64(f.header[8:], readSector)
	le.PutUint16(mem[avail+2:], 1)
	pos := header + 24
	if !shared {
		put(len(sizes)+1, pos, 1, descFlagWrite)
		f.status = &mem[pos]
	}
	pos += 4
	// Each descriptor is surrounded by guards and points below its predecessor.
	for i := len(sizes) - 1; i >= 0; i-- {
		length := sizes[i]
		data := mem[pos+1 : pos+1+length]
		f.segs[i] = data
		copy(data, bytes.Repeat([]byte{sentinel}, length))
		flags := descFlagWrite | descFlagNext
		if i == len(sizes)-1 && shared {
			flags = descFlagWrite
			f.status = &mem[pos+1+length]
			length++
		}
		put(i+1, pos+1, length, flags)
		f.guards = append(f.guards, &mem[pos], &mem[pos+1+length])
		pos += length + 2
	}
	*f.status = sentinel
	return f
}

func (f *windowFixture) check(t *testing.T, status byte, delivered int, pending bool) {
	t.Helper()
	if *f.status != status {
		t.Fatalf("status=%d want %d", *f.status, status)
	}
	var idx uint16 = 1
	wantLen := uint32(delivered + 1)
	if pending {
		idx = 0
		wantLen = 0
	}
	if got := binary.LittleEndian.Uint32(f.used[8:]); got != wantLen || f.queue.baseIdx != idx || binary.LittleEndian.Uint16(f.used[2:]) != idx {
		t.Fatalf("base=%d used=%x want idx=%d length=%d", f.queue.baseIdx, f.used, idx, wantLen)
	}
	pos := 0
	for _, seg := range f.segs {
		for _, got := range seg {
			want := byte(sentinel)
			if pos < delivered {
				want = f.backend.data[readSector*SectorSize+pos]
			}
			if want != got {
				t.Fatalf("payload[%d]=%x want %x", pos, got, want)
			}
			pos++
		}
	}
	for _, guard := range f.guards {
		if *guard != 0 {
			t.Fatal("payload overran guard")
		}
	}
	if len(f.queue.readScratch) > maxReadWindow || bytes.Count(f.queue.readScratch, []byte{0}) != len(f.queue.readScratch) {
		t.Fatal("dirty or oversized read workspace")
	}
	stats := f.server.stats.Snapshot().Read
	if stats.Count != uint64(idx) || (!pending && stats.Bytes != uint64(delivered)) {
		t.Fatalf("read stats: %+v", stats)
	}
}

func TestReadPayloadGeometry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sizes, windows  []int
		shared, scratch bool
	}{
		{"status-only", nil, nil, false, false},
		{"direct", []int{4096}, []int{4096}, true, false},
		{"64-pages", slices.Repeat([]int{4096}, 64), []int{64 * 4096}, false, true},
		{"mixed-1MiB", []int{512, 4096, maxReadWindow - 4608}, []int{maxReadWindow}, true, true},
		{"split-descriptor", []int{512, maxReadWindow}, []int{maxReadWindow, 512}, false, true},
		{"single-large", []int{maxReadWindow + 512}, []int{maxReadWindow, 512}, false, false},
		{"large-disjoint", []int{256 << 10, 256 << 10, 256 << 10, 256 << 10}, []int{256 << 10, 256 << 10, 256 << 10, 256 << 10}, false, false},
		{"small-tail", []int{256 << 10, 512, 4096}, []int{(256 << 10) + 4608}, true, true},
		{"large-then-fragments", []int{256 << 10, 512, 256 << 10, 4096}, []int{256 << 10, (256 << 10) + 4608}, false, true},
		{"three-windows", []int{512, maxReadWindow + 4096, maxReadWindow + 4096, 512}, []int{maxReadWindow, maxReadWindow, 9216}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowFixture(t, tc.sizes, tc.shared)
			// A later backend call must never see plaintext left by an
			// earlier window, including the unused tail of the scratch.
			f.backend.fault = func(p []byte, off int64) (int, error) {
				if bytes.Count(f.queue.readScratch, []byte{0}) != len(f.queue.readScratch) {
					t.Fatal("scratch not cleared before next window")
				}
				return f.backend.memBackend.ReadAt(p, off)
			}
			selections := 0
			f.server.backend = &selectingRequestBackend{Backend: f.backend, selectBackend: func() Backend {
				selections++
				if selections > 1 {
					t.Fatal("backend selected twice")
				}
				return f.backend
			}}
			if err := f.server.processQueue(f.queue); err != nil {
				t.Fatal(err)
			}
			done := 0
			var want []windowCall
			for _, length := range tc.windows {
				want = append(want, windowCall{int64(readSector*SectorSize + done), length})
				done += length
			}
			if selections != 1 || !slices.Equal(f.backend.calls, want) || (f.queue.readScratch != nil) != tc.scratch {
				t.Fatalf("selected=%d calls=%v scratch=%d", selections, f.backend.calls, len(f.queue.readScratch))
			}
			f.check(t, BlkStatusOK, done, false)
			if tc.scratch {
				ptr := &f.queue.readScratch[0]
				f.server.backend = f.backend
				if _, err := f.server.processChain(f.queue, 0); err != nil || ptr != &f.queue.readScratch[0] {
					t.Fatal("failed to reuse scratch", err)
				}
			}
		})
	}
}

func TestReadWindowResults(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, direct := range []bool{false, true} {
			const length = 4608
			for _, tc := range []struct {
				name      string
				n         int
				err       error
				status    byte
				delivered int
			}{
				{"full", length, nil, BlkStatusOK, length},
				{"eof", length, fmt.Errorf("wrapped: %w", io.EOF), BlkStatusOK, length},
				{"again", length, syscall.EAGAIN, BlkStatusOK, length},
				{"short", 777, nil, BlkStatusIOErr, 777},
				{"short-eof", 777, io.EOF, BlkStatusIOErr, 777},
				{"short-again", 777, syscall.EAGAIN, BlkStatusIOErr, 777},
				{"zero", 0, nil, BlkStatusIOErr, 0},
				{"error", 777, syscall.EIO, BlkStatusIOErr, 777},
				{"full-error", length, syscall.EIO, BlkStatusIOErr, length},
				{"joined-error", length, errors.Join(io.EOF, syscall.EIO), BlkStatusIOErr, length},
				{"negative", -1, nil, BlkStatusIOErr, 0},
				{"too-long", length + 1, nil, BlkStatusIOErr, 0},
			} {
				t.Run(fmt.Sprintf("%s/direct=%t/shared=%t", tc.name, direct, shared), func(t *testing.T) {
					sizes := []int{512, 4096}
					if direct {
						sizes = []int{length}
					}
					f := newWindowFixture(t, sizes, shared)
					f.backend.fault = func(p []byte, off int64) (int, error) {
						if !direct {
							for i := range p {
								p[i] = 0xee
							}
						}
						copy(p[:tc.delivered], f.backend.data[off:])
						return tc.n, tc.err
					}
					if err := f.server.processQueue(f.queue); err != nil {
						t.Fatal(err)
					}
					if len(f.backend.calls) != 1 {
						t.Fatalf("calls=%v", f.backend.calls)
					}
					f.check(t, tc.status, tc.delivered, false)
				})
			}
		}
	}
}

func TestSecondReadWindowFails(t *testing.T) {
	f := newWindowFixture(t, []int{4096, maxReadWindow, maxReadWindow, 4096}, true)
	f.backend.fault = func(p []byte, off int64) (int, error) {
		if len(f.backend.calls) == 2 {
			const prefix = 4608
			copy(p[:prefix], f.backend.data[off:])
			return prefix, syscall.EAGAIN
		}
		return f.backend.memBackend.ReadAt(p, off)
	}
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	if len(f.backend.calls) != 2 {
		t.Fatalf("calls=%v", f.backend.calls)
	}
	f.check(t, BlkStatusIOErr, maxReadWindow+4608, false)
}

// Observe cancellation exactly between scatter copies, on the worker thread.
type cancelOnScatter struct {
	context.Context
	first  []byte
	cancel context.CancelFunc
}

func (c cancelOnScatter) Err() error {
	if c.first[0] != sentinel {
		c.cancel()
	}
	return c.Context.Err()
}

func TestGatherReadTermination(t *testing.T) {
	for _, mode := range []string{"terminalEOF", "terminalAgain", "stopAfterRead", "stopDuringScatter"} {
		t.Run(mode, func(t *testing.T) {
			f := newWindowFixture(t, []int{512, 4096, 4096}, true)
			if mode == "stopDuringScatter" {
				f.queue.ctx = cancelOnScatter{f.queue.ctx, f.segs[0], f.queue.cancel}
			}
			f.backend.fault = func(p []byte, off int64) (int, error) {
				n, err := f.backend.memBackend.ReadAt(p, off)
				switch mode {
				case "terminalEOF":
					err = readretry.Terminal(io.EOF)
				case "terminalAgain":
					err = readretry.Terminal(syscall.EAGAIN)
				case "stopAfterRead":
					f.queue.stopReading()
				}
				return n, err
			}
			if err := f.server.processQueue(f.queue); !readretry.IsTerminal(err) {
				t.Fatalf("err=%v", err)
			}
			done := 0
			if mode == "stopDuringScatter" {
				done = 512
			}
			if len(f.backend.calls) != 1 {
				t.Fatal(f.backend.calls)
			}
			f.check(t, sentinel, done, true)
		})
	}
}

func TestReadRange(t *testing.T) {
	for _, sector := range []uint64{readSector + 1, math.MaxInt64 / SectorSize, math.MaxUint64} {
		f := newWindowFixture(t, []int{512, 4096}, false)
		binary.LittleEndian.PutUint64(f.header[8:], sector)
		if err := f.server.processQueue(f.queue); err != nil {
			t.Fatal(err)
		}
		if len(f.backend.calls) != 0 {
			t.Fatal("out-of-range disk read")
		}
		f.check(t, BlkStatusIOErr, 0, false)
	}
	// Repeated references to one small mapping can overflow used.len.
	seg := make([]byte, 1<<20)
	segs := slices.Repeat([][]byte{seg}, 4096)
	q := &virtq{}
	if n, err := q.readPayload(nil, 0, segs); n != 0 || err == nil {
		t.Fatalf("invalid used length: %d %v", n, err)
	}
}

func TestLargeSGWriteThenRead(t *testing.T) {
	f := newWindowFixture(t, slices.Repeat([]int{16 << 10}, 64), false)
	mem := f.server.memTable.regions[0].mmapBytes
	le := binary.LittleEndian
	le.PutUint32(f.header, BlkTypeOut)
	offset := readSector * SectorSize
	want := bytes.Clone(f.backend.data[offset:])
	for i, seg := range f.segs {
		le.PutUint16(mem[(i+1)*16+12:], descFlagNext)
		copy(seg, want[i*len(seg):])
	}
	clear(f.backend.data)
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	if *f.status != BlkStatusOK || le.Uint32(f.used[8:]) != 1 || !bytes.Equal(want, f.backend.data[offset:]) {
		t.Fatal("large multi-segment write did not commit the expected bytes")
	}
	for i, seg := range f.segs {
		clear(seg)
		le.PutUint16(mem[(i+1)*16+12:], descFlagNext|descFlagWrite)
	}
	le.PutUint32(f.header, BlkTypeIn)
	le.PutUint16(mem[f.queue.availAddr-f.queue.descAddr+2:], 2)
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	if *f.status != BlkStatusOK || f.queue.baseIdx != 2 || le.Uint32(mem[f.queue.usedAddr-f.queue.descAddr+16:]) != 1+maxReadWindow {
		t.Fatal("readback completion mismatch")
	}
	for i, seg := range f.segs {
		if !bytes.Equal(seg, want[i*len(seg):(i+1)*len(seg)]) {
			t.Fatalf("bad segment %d", i)
		}
	}
}

func TestMalformedDirectChain(t *testing.T) {
	for _, next := range []uint16{1, 128} {
		f := newWindowFixture(t, []int{512}, false)
		binary.LittleEndian.PutUint16(f.server.memTable.regions[0].mmapBytes[16+14:], next)
		if _, err := f.server.processChain(f.queue, 0); err == nil || len(f.backend.calls) != 0 {
			t.Fatal("accepted invalid descriptor chain")
		}
	}
}

func TestReadCannotExposeTrailingPartialSector(t *testing.T) {
	f := newWindowFixture(t, []int{512}, true)
	f.backend.data = bytes.Clone(f.backend.data[:readSector*SectorSize+1])
	// One payload byte plus the shared status. This byte is beyond the
	// capacity exported to the guest even though it exists on the source.
	binary.LittleEndian.PutUint32(f.server.memTable.regions[0].mmapBytes[16+8:], 2)
	if err := f.server.processQueue(f.queue); err != nil {
		t.Fatal(err)
	}
	if len(f.backend.calls) != 0 || f.segs[0][1] != BlkStatusIOErr || binary.LittleEndian.Uint32(f.used[8:]) != 1 {
		t.Fatal("partial sector beyond advertised capacity was delivered")
	}
}

func TestIndirectChainLengthIsBounded(t *testing.T) {
	for _, length := range []uint32{0, 17, 128 * 16} {
		f := newWindowFixture(t, []int{512}, false)
		desc := f.server.memTable.regions[0].mmapBytes[16:]
		binary.LittleEndian.PutUint16(desc[12:], descFlagIndirect)
		binary.LittleEndian.PutUint32(desc[8:], length)
		if _, err := f.server.processChain(f.queue, 0); err == nil || len(f.backend.calls) != 0 {
			t.Fatal("accepted invalid indirect table")
		}
	}
}

func TestContiguousDescriptorsReadDirectly(t *testing.T) {
	b := &windowBackend{memBackend: &memBackend{data: bytes.Repeat([]byte{0x71}, 1<<20)}}
	s, q, _, data := segmentQueue(b, BlkTypeIn, []int{512, 4096, (1 << 20) - 4608})
	// Move the status into the last data descriptor. It must not be part of p.
	last := s.memTable.regions[0].mmapBytes[3*16:]
	binary.LittleEndian.PutUint32(last[8:], binary.LittleEndian.Uint32(last[8:])+1)
	binary.LittleEndian.PutUint16(last[12:], descFlagWrite)
	b.fault = func(p []byte, off int64) (int, error) {
		if &p[0] != &data[0] || len(p) != 1<<20 || cap(p) != len(p) {
			t.Fatal("wanted exact direct window")
		}
		return b.memBackend.ReadAt(p, off)
	}
	n, err := s.processChain(q, 0)
	if err != nil || n != 1+(1<<20) || q.readScratch != nil || !bytes.Equal(b.data, data[:len(b.data)]) || data[len(b.data)] != BlkStatusOK {
		t.Fatal("incorrect contiguous direct read")
	}
	// Even adjacent addresses cannot extend the first slice's mapping.
	first := data[:512:512]
	if contiguousWindow([][]byte{first, data[512:4608]}, 4608) != nil {
		t.Fatal("extended past mapping")
	}
}
