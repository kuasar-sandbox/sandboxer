package vhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

func TestMemoryTranslationRequiresLoaderAndValidCoverage(t *testing.T) {
	mem := bytes.Repeat([]byte{0xa5}, 8192)
	table := MemTable{regions: []MemRegion{{GuestPhysAddr: 0x1000, UserspaceAddr: 0x10000, MemorySize: 8192, MmapOffset: 4096, mmapBytes: mem}}}
	if b, err := table.TranslateGPA(context.Background(), 0x1000, 1); b != nil || !readretry.IsTerminal(err) {
		t.Fatalf("unbound callback: %v %v", b, err)
	}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, true)
	var seen [][2]uint64
	table.EnsureLoaded = func(c context.Context, off, n uint64) error {
		if c.Value(key{}) != true {
			t.Fatal("caller context was lost")
		}
		seen = append(seen, [2]uint64{off, n})
		return nil
	}
	for _, uva := range []bool{false, true} {
		translate := table.TranslateGPA
		base := uint64(0x1000)
		if uva {
			translate = table.TranslateUVA
			base = 0x10000
		}
		b, err := translate(ctx, base+4095, 2)
		if err != nil || len(b) != 2 || b[0] != 0xa5 {
			t.Fatalf("cross-page range: %v %v", b, err)
		}
		for _, r := range [][2]uint64{{base - 1, 1}, {base + 8192, 1}, {^uint64(0), 2}, {base, ^uint64(0)}} {
			if b, err := translate(ctx, r[0], r[1]); b != nil || !readretry.IsTerminal(err) {
				t.Fatalf("invalid range exposed: %v %v", r, err)
			}
		}
	}
	if len(seen) != 2 || seen[0] != [2]uint64{8191, 2} || seen[1] != seen[0] {
		t.Fatalf("memfd offsets=%v", seen)
	}
	table.regions[0].mmapBytes = mem[:1]
	if _, err := table.TranslateGPA(ctx, 0x1001, 1); err == nil {
		t.Fatal("accepted missing slab coverage")
	}
	table.regions[0].mmapBytes = mem
	table.regions[0].MmapOffset = ^uint64(0)
	if _, err := table.TranslateGPA(ctx, 0x1001, 1); err == nil {
		t.Fatal("accepted offset overflow")
	}
}

func TestMandatoryMemoryFailureCannotPublishUsed(t *testing.T) {
	// Every translation boundary is mandatory, including read-side rings,
	// descriptors, headers, device-readable data, writable data and status.
	for _, write := range []bool{false, true} {
		for _, off := range []uint64{128, 0, 256, 16, 512, 32, 1536, 192, 196} {
			t.Run(fmt.Sprintf("write_%t_offset_%d", write, off), func(t *testing.T) {
				stream := recoveryStream{read: func(_ context.Context, p []byte) (int, error) { clear(p); return len(p), nil }}
				s, q, mem, _ := recoveryQueue(t, stream, write)
				failure := errors.New("population failed")
				s.SetMemoryLoader(func(ctx context.Context, offset, length uint64) error {
					if offset == off {
						return failure
					}
					return ctx.Err()
				})
				err := s.processQueue(q)
				if !readretry.IsTerminal(err) || !errors.Is(err, failure) {
					t.Fatalf("err=%v", err)
				}
				if q.baseIdx != 0 || binary.LittleEndian.Uint16(mem[194:]) != 0 || !bytes.Equal(mem[196:204], make([]byte, 8)) {
					t.Fatal("memory failure published used entry")
				}
			})
		}
	}
}

func TestIndirectReadMemoryIsPrepared(t *testing.T) {
	stream := recoveryStream{read: func(_ context.Context, p []byte) (int, error) { return len(p), nil }}
	s, q, mem, _ := recoveryQueue(t, stream, false)
	// Move the descriptor table into an indirect GPA table; the header remains
	// device-readable and must be loaded before ParseBlkReqHeader accesses it.
	copy(mem[2048:2096], mem[:48])
	clear(mem[:48])
	binary.LittleEndian.PutUint64(mem, 2048)
	binary.LittleEndian.PutUint32(mem[8:], 48)
	binary.LittleEndian.PutUint16(mem[12:], descFlagIndirect)
	for _, failOff := range []uint64{2048, 256, 512, 1536} {
		s.SetMemoryLoader(func(_ context.Context, off, n uint64) error {
			if off == failOff {
				return errors.New("indirect load failed")
			}
			return nil
		})
		if err := s.processQueue(q); !readretry.IsTerminal(err) {
			t.Fatalf("offset %d: %v", failOff, err)
		}
		assertPending(t, q, mem)
	}
}

func TestQueueResetCancelsMemoryLoadBeforeTableReplacement(t *testing.T) {
	s, q, mem, _ := recoveryQueue(t, recoveryStream{}, false)
	entered := make(chan struct{})
	s.SetMemoryLoader(func(ctx context.Context, _, _ uint64) error { close(entered); <-ctx.Done(); return ctx.Err() })
	s.queues[0] = q
	q.done = make(chan struct{})
	done := make(chan error, 1)
	go func() { defer close(q.done); done <- s.processQueue(q) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("did not enter load")
	}
	reset := make(chan struct{})
	go func() { s.resetConnectionState(); close(reset) }()
	select {
	case <-reset:
	case <-time.After(time.Second):
		t.Fatal("reset did not cancel/drain memory load")
	}
	if err := <-done; !errors.Is(err, context.Canceled) || !readretry.IsTerminal(err) {
		t.Fatal(err)
	}
	if len(s.memTable.regions) != 0 {
		t.Fatal("table not reset")
	}
	assertPending(t, q, mem)
}
