package vhost

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestVringOrderIndex(t *testing.T) {
	for _, offset := range []int{0, 2} {
		t.Run(fmt.Sprintf("offset%d", offset), func(t *testing.T) {
			storage := [4]uint64{}
			mem := unsafe.Slice((*byte)(unsafe.Pointer(&storage[0])), 32)
			hdr := mem[offset : offset+6]
			copy(hdr, []byte{0x5a, 0xa5, 0x34, 0x12})
			hdr[4], hdr[5] = 0xde, 0xad
			before := append([]byte(nil), mem...)
			got, err := loadAvailIdxAcquire(hdr, uint64(0x1000+offset))
			if err != nil {
				t.Fatal(err)
			}
			if got != 0x1234 {
				t.Fatalf("idx=%#x", got)
			}
			if !bytes.Equal(mem, before) {
				t.Fatal("available ring modified by load")
			}
			if offset != 0 {
				if _, err := loadAvailIdxAcquire(hdr[:4], uint64(0x1000+offset)); err == nil {
					t.Fatal("accepted truncated unaligned-word layout")
				}
				return
			}
			for _, idx := range []uint16{0xffff, 0, 0x5678} {
				binary.LittleEndian.PutUint16(hdr[2:], idx-1)
				header, err := loadUsedHeaderAcquire(hdr, 0x1000)
				if err != nil {
					t.Fatal(err)
				}
				if got := header.index(); got != idx-1 {
					t.Fatalf("used idx=%#x want %#x", got, idx-1)
				}
				header.publishNextRelease()
				if got := binary.LittleEndian.Uint16(hdr[2:]); got != idx {
					t.Fatalf("idx=%#x want %#x", got, idx)
				}
				if hdr[0] != 0x5a || hdr[1] != 0xa5 {
					t.Fatal("flags overwritten")
				}
				if hdr[4] != 0xde || hdr[5] != 0xad {
					t.Fatal("bytes following used header overwritten")
				}
			}
		})
	}
}

func TestVringOrderQueueAlignment(t *testing.T) {
	storage := [64]uint64{}
	mem := unsafe.Slice((*byte)(unsafe.Pointer(&storage[0])), 512)
	const uva = uint64(0x1000)
	s := &Server{}
	s.memTable.SetRegions([]MemRegion{{UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
	// A 2-byte aligned, non-4-byte aligned available ring is valid.
	binary.LittleEndian.PutUint16(mem[4:6], 1)
	binary.LittleEndian.PutUint16(mem[6:8], 7)
	q := &virtq{num: 8, availAddr: uva + 2, usedAddr: uva + 128}
	got, err := s.readAvailRing(q)
	if err != nil || got.idx != 1 || got.ring[0] != 7 {
		t.Fatalf("avail=%+v err=%v", got, err)
	}
	mem[128], mem[129] = 0x5a, 0xa5
	if err := s.publishUsed(q, 7, 513); err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(mem[130:132]) != 1 || binary.LittleEndian.Uint32(mem[132:136]) != 7 || binary.LittleEndian.Uint32(mem[136:140]) != 513 {
		t.Fatal("incorrect completion")
	}
	if mem[128] != 0x5a || mem[129] != 0xa5 {
		t.Fatal("used flags overwritten")
	}
	q.availAddr = uva + 1
	if _, err := s.readAvailRing(q); err == nil {
		t.Fatal("accepted misaligned available ring")
	}
	for _, off := range []uint64{129, 130} {
		q.usedAddr = uva + off
		before := append([]byte(nil), mem...)
		if err := s.publishUsed(q, 7, 513); err == nil {
			t.Fatal("accepted misaligned used ring")
		}
		if !bytes.Equal(mem, before) {
			t.Fatal("invalid used ring was modified")
		}
	}
	if _, err := loadAvailIdxAcquire(mem[:3], uva); err == nil {
		t.Fatal("accepted short header")
	}
	if _, err := loadAvailIdxAcquire(mem[1:5], uva); err == nil {
		t.Fatal("accepted misaligned host mapping")
	}
	if _, err := loadUsedHeaderAcquire(mem[:3], uva); err == nil {
		t.Fatal("accepted short used header")
	}
	if _, err := loadUsedHeaderAcquire(mem[2:6], uva); err == nil {
		t.Fatal("accepted misaligned used host mapping")
	}
}

// The minimum valid queue needs exactly six bytes for its available ring.
// Use a capped mapping at a page boundary to exercise the widened load's bounds.
func TestVringOrderAvailMappingEnd(t *testing.T) {
	mem, err := syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Munmap(mem)
	tests := []struct {
		name   string
		offset int
	}{
		{name: "flags_and_index_word", offset: 4088},
		{name: "index_and_first_entry_word_at_page_end", offset: 4090},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			off := tt.offset
			hdr := mem[off : off+6 : off+6]
			const uva = uint64(0x1000)
			s := &Server{}
			s.memTable.SetRegions([]MemRegion{{UserspaceAddr: uva + uint64(off), MemorySize: 6, mmapBytes: hdr}})
			q := &virtq{num: 1, availAddr: uva + uint64(off)}
			for _, idx := range []uint16{0xfffe, 0xffff, 0, 1} {
				binary.LittleEndian.PutUint16(hdr, idx^0xaaaa)
				binary.LittleEndian.PutUint16(hdr[2:], idx)
				binary.LittleEndian.PutUint16(hdr[4:], idx^0x5555)
				before := append([]byte(nil), mem...)
				got, err := s.readAvailRing(q)
				if err != nil || got.idx != idx || got.flags != idx^0xaaaa || len(got.ring) != 1 || got.ring[0] != idx^0x5555 {
					t.Fatalf("idx=%d avail=%+v err=%v", idx, got, err)
				}
				if !bytes.Equal(before, mem) {
					t.Fatal("available-ring read changed memory")
				}
			}
		})
	}
}

func TestVringOrderUsedWraparound(t *testing.T) {
	storage := [16]uint64{}
	mem := unsafe.Slice((*byte)(unsafe.Pointer(&storage[0])), 128)
	const uva = uint64(0x1000)
	s := &Server{}
	s.memTable.SetRegions([]MemRegion{{UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
	q := &virtq{num: 2, usedAddr: uva}
	binary.LittleEndian.PutUint16(mem[2:], 0xffff)
	// Ordered steps retain the queue state across wraparound.
	tests := []struct {
		name     string
		head     uint16
		flags    uint16
		length   uint32
		wantIdx  uint16
		wantSlot int
	}{
		{name: "wrap_to_zero", head: 0, flags: 1, length: 511, wantIdx: 0, wantSlot: 1},
		{name: "continue_after_wrap", head: 1, flags: 2, length: 512, wantIdx: 1, wantSlot: 0},
	}
	for _, tt := range tests {
		if !t.Run(tt.name, func(t *testing.T) {
			// Serialized device-owned flag change between completions.
			binary.LittleEndian.PutUint16(mem, tt.flags)
			before := append([]byte(nil), mem...)
			if err := s.publishUsed(q, tt.head, tt.length); err != nil {
				t.Fatal(err)
			}
			expected := append([]byte(nil), before...)
			binary.LittleEndian.PutUint16(expected[2:], tt.wantIdx)
			slot := 4 + tt.wantSlot*8
			binary.LittleEndian.PutUint32(expected[slot:], uint32(tt.head))
			binary.LittleEndian.PutUint32(expected[slot+4:], tt.length)
			if !bytes.Equal(mem, expected) {
				t.Fatal("completion changed wrong slot, flags, or adjacent bytes")
			}
		}) {
			return
		}
	}
}

// Exercise publication through mmap memory, as in production. The race detector
// cannot model guest accesses; this tests host publication, not mixed-width guest
// interoperability or a proof of ordering. Acknowledgment prevents the writer
// from reusing the payload until the reader has checked every byte. The only
// writer-to-reader publication within the loop is the vring index itself.
func TestVringOrderPublication(t *testing.T) {
	for _, offset := range []int{0, 2} {
		t.Run(fmt.Sprintf("offset%d", offset), func(t *testing.T) {
			mem, err := syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_SHARED)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.Munmap(mem)
			published := mem[offset : offset+6]
			loadPublished := func() (uint16, error) {
				return loadAvailIdxAcquire(published, uint64(offset))
			}
			// The simulated guest accesses aligned words directly; production
			// available-ring access exposes no writable word.
			publishedWord := (*uint32)(unsafe.Pointer(&mem[(offset+2)&^3]))
			ack := mem[64:68]
			ackWord := (*uint32)(unsafe.Pointer(&ack[0]))
			loadAck := func() (uint16, error) {
				return uint16(atomic.LoadUint32(ackWord) >> 16), nil
			}
			payload := mem[128:384]
			const rounds = 70000 // Includes 16-bit index wraparound.
			deadline := time.Now().Add(15 * time.Second)
			wait := func(load func() (uint16, error), want uint16) error {
				for {
					got, err := load()
					if err != nil {
						return err
					}
					if got == want {
						return nil
					}
					if time.Now().After(deadline) {
						return fmt.Errorf("timeout waiting for index %d", want)
					}
					runtime.Gosched()
				}
			}
			done := make(chan error, 1)
			go func() {
				for i := 1; i <= rounds; i++ {
					if err := wait(loadAck, uint16(i-1)); err != nil {
						done <- err
						return
					}
					for j := range payload {
						payload[j] = byte(i + j)
					}
					// Test producer uses a full atomic word in either layout; this
					// covers both extraction branches, not guest 16-bit stores.
					word := uint32(uint16(i)) | uint32(uint16(i)^0xa5a5)<<16
					if offset == 0 {
						word = word<<16 | word>>16
					}
					atomic.StoreUint32(publishedWord, word)
				}
				done <- nil
			}()
			// Always join before unmapping, including on a reader failure.
			defer func() {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			for i := 1; i <= rounds; i++ {
				if err := wait(loadPublished, uint16(i)); err != nil {
					t.Fatal(err)
				}
				for j, got := range payload {
					if want := byte(i + j); got != want {
						t.Fatalf("index=%d payload[%d]=%d want %d", i, j, got, want)
					}
				}
				header, err := loadUsedHeaderAcquire(ack, 64)
				if err != nil {
					t.Fatal(err)
				}
				header.publishNextRelease()
			}
		})
	}
}
