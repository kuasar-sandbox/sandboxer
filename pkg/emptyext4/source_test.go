package emptyext4

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"math/rand"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func fixed(t *testing.T, size int64) *Source {
	t.Helper()
	s, err := newWithIdentity(size, [16]byte{1, 2, 3, 4, 5, 6, 0x47, 8, 0x89, 10, 11, 12, 13, 14, 15, 16}, 1700000000)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInvalidCapacityAndGeometryCoverage(t *testing.T) {
	for _, size := range []int64{math.MinInt64, -4096, -1, 0, MinSize - 4096, MinSize - 1, MinSize + 1, MaxSize + 1, MaxSize + 4096, math.MaxInt64} {
		if _, err := New(size); err == nil {
			t.Errorf("accepted %d", size)
		}
	}
	sizes := []int64{MinSize, MinSize + 4096, 8 << 20, 16 << 20, 32 << 20, 64 << 20, (128 << 20) - 4096, 128 << 20, (128 << 20) + 4096, (128 << 20) + 8192, 650 << 20, 1 << 30, 32 << 30, (512 << 30) + 4096, MaxSize - (128 << 20) + 4096, MaxSize - 4096, MaxSize}
	for _, size := range sizes {
		s := fixed(t, size)
		if s.Size() != uint64(size) || s.blocks*BlockSize != uint64(size) {
			t.Fatal("capacity changed")
		}
		var free uint64
		for i, g := range s.groups {
			used := uint64(0)
			if uint64(i)%groupsPerFlex == 0 {
				used = min(groupsPerFlex, uint64(len(s.groups)-i)) * (2 + s.tableBlocks)
			}
			if g.hasSuper {
				used += 1 + s.gdtBlocks
			}
			if i == 0 {
				used++
			}
			if g.count <= used || g.start+g.count > s.blocks {
				t.Fatalf("invalid group at size=%d group=%d", size, i)
			}
			free += g.count - used
		}
		if free != s.freeBlocks {
			t.Fatal("free block accounting")
		}
	}
	// Pure geometry fuzz avoids retaining 10,000 large layout objects.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 10000; i++ {
		blocks := uint64(MinSize)/BlockSize + uint64(rng.Int63n((MaxSize-MinSize)/int64(BlockSize)+1))
		count, bpg, ipg, table, gdt, err := geometry(blocks)
		if err != nil {
			t.Fatal(err)
		}
		if bpg%8 != 0 || bpg > 32768 || ceilDiv(blocks, bpg) != count || ipg%16 != 0 || table*16 != ipg {
			t.Fatalf("invalid geometry for %d", blocks)
		}
		last := blocks - (count-1)*bpg
		used := 2 + table
		if hasSuper(count - 1) {
			used += 1 + gdt
		}
		if count == 1 {
			used++
		}
		if last <= used {
			t.Fatalf("tail cannot hold metadata for %d", blocks)
		}
	}
	// Every nominal 128 MiB group-count transition in the full supported
	// range, including tails around bitmap/table/descriptor sizes.
	cases := 0
	for boundary := uint64(1); boundary <= uint64(MaxSize)/BlockSize/maxBlocksPerGroup; boundary++ {
		for _, delta := range []int64{-1154, -1025, -1024, -513, -512, -129, -128, -65, -64, -2, -1, 0, 1, 2, 64, 65, 128, 129, 512, 513, 1024, 1025, 1154} {
			blocks := int64(boundary*maxBlocksPerGroup) + delta
			if blocks < MinSize/int64(BlockSize) || blocks > MaxSize/int64(BlockSize) {
				continue
			}
			count, bpg, ipg, table, gdt, err := geometry(uint64(blocks))
			if err != nil {
				t.Fatal(err)
			}
			if bpg%8 != 0 || bpg > maxBlocksPerGroup || ceilDiv(uint64(blocks), bpg) != count || ipg%16 != 0 || ipg > BlockSize*8 || table*16 != ipg {
				t.Fatalf("invalid geometry for %d", blocks)
			}
			last := uint64(blocks) - (count-1)*bpg
			used := 2 + table
			if hasSuper(count - 1) {
				used += 1 + gdt
			}
			if count == 1 {
				used++
			}
			if last <= used || bpg <= 2+table+1+gdt+1 {
				t.Fatalf("insufficient group space for %d", blocks)
			}
			cases++
		}
	}
	t.Logf("geometry boundary cases=%d; random cases=10000", cases)
}

func TestFieldsAndBitmapAccounting(t *testing.T) {
	for _, size := range []int64{MinSize, (128 << 20) + 4096, 650 << 20, 32 << 30, MaxSize} {
		s := fixed(t, size)
		var sb [1024]byte
		if n, e := s.ReadAt(context.Background(), sb[:], 1024); n != len(sb) || e != nil {
			t.Fatal(n, e)
		}
		if binary.LittleEndian.Uint16(sb[0x38:]) != 0xef53 || binary.LittleEndian.Uint32(sb[4:]) != uint32(s.blocks) {
			t.Fatal("superblock identity/capacity")
		}
		if binary.LittleEndian.Uint32(sb[0x5c:]) != 0x28 || binary.LittleEndian.Uint32(sb[0x60:]) != 0x2c2 || binary.LittleEndian.Uint32(sb[0x64:]) != 0x6b || sb[0x174] != 3 {
			t.Fatal("feature flags")
		}
		if !bytes.Equal(sb[0x68:0x78], s.uuid[:]) {
			t.Fatal("uuid")
		}
		var page [4096]byte
		for i, g := range s.groups {
			if _, err := s.ReadAt(context.Background(), page[:], g.blockBitmap*BlockSize); err != nil {
				t.Fatal(err)
			}
			set := uint64(0)
			for _, v := range page {
				set += uint64(bits.OnesCount8(v))
			}
			if want := BlockSize*8 - uint64(g.freeBlocks); set != want {
				t.Fatalf("block bitmap size=%d group=%d set=%d want=%d", size, i, set, want)
			}
			if _, err := s.ReadAt(context.Background(), page[:], g.inodeBitmap*BlockSize); err != nil {
				t.Fatal(err)
			}
			set = 0
			for _, v := range page {
				set += uint64(bits.OnesCount8(v))
			}
			want := BlockSize*8 - s.inodesPerGroup
			if i == 0 {
				want += 10
			}
			if set != want {
				t.Fatalf("inode bitmap size=%d group=%d set=%d want=%d", size, i, set, want)
			}
			var gd [64]byte
			if _, err := s.ReadAt(context.Background(), gd[:], BlockSize+uint64(i)*64); err != nil {
				t.Fatal(err)
			}
			if binary.LittleEndian.Uint16(gd[0x12:]) != 4 {
				t.Fatal("inode tables not marked zeroed")
			}
			if binary.LittleEndian.Uint16(gd[0x1c:]) != 0 {
				t.Fatal("itable_unused requires checksum feature")
			}
		}
	}
}

func TestArbitraryReadsAndConcurrency(t *testing.T) {
	// Keep the dense oracle small; correctness of bytes is independently
	// checked by e2fsck and Linux. This test checks read slicing/concurrency.
	s := fixed(t, 8<<20)
	all := make([]byte, s.Size())
	if n, e := s.ReadAt(context.Background(), all, 0); n != len(all) || e != nil {
		t.Fatal(n, e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			var buf [16385]byte
			for i := 0; i < 1000; i++ {
				off := uint64(rng.Int63n(int64(s.Size())))
				// Half the samples deliberately target metadata boundaries.
				if i%2 == 0 {
					e := s.data[rng.Intn(len(s.data))]
					off = e.Offset + uint64(rng.Intn(int(min(e.Size, 8192))))
				}
				length := rng.Intn(len(buf))
				n, e := s.ReadAt(context.Background(), buf[:length], off)
				want := min(length, len(all)-int(off))
				if n != want || (e != nil && !errors.Is(e, io.EOF)) || !bytes.Equal(buf[:n], all[off:off+uint64(n)]) {
					errs <- fmt.Errorf("off=%d len=%d n=%d err=%v", off, length, n, e)
					return
				}
				r, e := s.RunAt(off, max(uint64(length), 1))
				if e != nil || r.Offset() != off || r.End() <= off || r.End() > min(s.Size(), off+max(uint64(length), 1)) {
					errs <- fmt.Errorf("bad run off=%d err=%v", off, e)
					return
				}
			}
		}(int64(worker + 10))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Every byte-sized fragment of each encoded structure matches whole read.
	for _, e := range s.data {
		for at := e.Offset; at < e.Offset+e.Size; at++ {
			var one [1]byte
			if _, err := s.ReadAt(context.Background(), one[:], at); err != nil || one[0] != all[at] {
				t.Fatalf("one-byte read at %d", at)
			}
		}
	}
}

func TestReadAndRunBoundaries(t *testing.T) {
	s := fixed(t, MinSize)
	var p [20]byte
	if n, e := s.ReadAt(context.Background(), p[:], s.Size()-7); n != 7 || !errors.Is(e, io.EOF) {
		t.Fatal(n, e)
	}
	for _, off := range []uint64{s.Size(), s.Size() + 1, math.MaxUint64} {
		if n, e := s.ReadAt(context.Background(), p[:], off); n != 0 || !errors.Is(e, io.EOF) {
			t.Fatal(off, n, e)
		}
		if _, e := s.RunAt(off, 1); !errors.Is(e, io.EOF) {
			t.Fatal(off, e)
		}
		if n, e := s.ReadAt(context.Background(), nil, off); n != 0 || !errors.Is(e, io.EOF) {
			t.Fatal("empty source EOF", off, n, e)
		}
	}
	if _, e := s.RunAt(0, 0); e == nil {
		t.Fatal("zero limit")
	}
	r, e := s.RunAt(0, math.MaxUint64)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.ReadAt(context.Background(), p[:], r.End()-r.Offset()-1); e == nil {
		t.Fatal("run overread")
	}
	if n, e := r.ReadAt(context.Background(), nil, r.End()-r.Offset()); n != 0 || e != nil {
		t.Fatal("empty run-end read", n, e)
	}
	last, err := s.RunAt(s.Size()-1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, e := last.ReadAt(context.Background(), nil, 1); n != 0 || e != nil {
		t.Fatal("empty final-run end", n, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, e := s.ReadAt(ctx, p[:], 0); n != 0 || !errors.Is(e, context.Canceled) {
		t.Fatal(n, e)
	}
	for at := uint64(0); at < s.Size(); {
		run, err := s.RunAt(at, s.Size()-at)
		if err != nil {
			t.Fatal(err)
		}
		if run.Kind() == sparse.Hole {
			copy(p[:], bytes.Repeat([]byte{0xa5}, len(p)))
			if _, err = s.ReadAt(context.Background(), p[:], at); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p[:], make([]byte, len(p))) {
				t.Fatal("hole not zero")
			}
		}
		at = run.End()
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAt(context.Background(), p[:], 0); err == nil {
		t.Fatal("read after close")
	}
	if _, err := s.RunAt(0, 1); err == nil {
		t.Fatal("run after close")
	}
}

func TestReadAtDoesNotAllocatePayload(t *testing.T) {
	s := fixed(t, 32<<30)
	var p [4096]byte
	off := s.groups[0].blockBitmap * BlockSize
	allocations := testing.AllocsPerRun(1000, func() {
		if _, err := s.ReadAt(context.Background(), p[:], off); err != nil {
			panic(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("ReadAt allocated %f times", allocations)
	}
	if s.Info().PrebuiltPayloadBytes != 0 {
		t.Fatal("prebuilt payload")
	}
}

func TestInodeTimestampEpoch(t *testing.T) {
	for _, stamp := range []uint32{1700000000, 0x7fffffff, 0x80000000, 0xffffffff} {
		s, err := newWithIdentity(MinSize, [16]byte{1}, stamp)
		if err != nil {
			t.Fatal(err)
		}
		var inode [256]byte
		if _, err = s.ReadAt(context.Background(), inode[:], s.groups[0].inodeTable*BlockSize+256); err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]int{{8, 0x8c}, {12, 0x84}, {16, 0x88}, {0x90, 0x94}} {
			sec := int64(int32(binary.LittleEndian.Uint32(inode[pair[0]:])))
			epoch := int64(binary.LittleEndian.Uint32(inode[pair[1]:]) & 3)
			if sec+(epoch<<32) != int64(stamp) {
				t.Fatal("timestamp epoch", stamp, pair)
			}
		}
	}
}

func BenchmarkConstruct(b *testing.B) {
	for _, size := range []int64{64 << 20, 32 << 30, 1 << 40} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				s, e := New(size)
				if e != nil {
					b.Fatal(e)
				}
				s.Close()
			}
		})
	}
}
func BenchmarkEncodeMetadata(b *testing.B) {
	for _, size := range []int64{64 << 20, 32 << 30, 1 << 40} {
		b.Run(fmt.Sprintf("%d", size), func(b *testing.B) {
			s, e := New(size)
			if e != nil {
				b.Fatal(e)
			}
			buf := make([]byte, 1<<20)
			b.ReportAllocs()
			b.SetBytes(int64(s.Info().DataBytes))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, extent := range s.data {
					for at := extent.Offset; at < extent.Offset+extent.Size; {
						n := min(uint64(len(buf)), extent.Offset+extent.Size-at)
						if _, e := s.ReadAt(context.Background(), buf[:n], at); e != nil {
							b.Fatal(e)
						}
						at += n
					}
				}
			}
		})
	}
}
