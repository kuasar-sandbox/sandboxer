package vhost

import (
	"bytes"
	"strings"
	"testing"
)

func TestStats_Record_BasicCounters(t *testing.T) {
	s := NewStats("blk0", "/tmp/blk0", 1<<20)   // 1 MiB
	s.Record(BlkTypeIn, 4096, 100_000, true)    // 100µs ok
	s.Record(BlkTypeIn, 8192, 250_000, true)    // 250µs ok
	s.Record(BlkTypeIn, 4096, 9_000_000, false) // 9ms err

	snap := s.Snapshot()
	if snap.Read.Count != 3 {
		t.Errorf("Read.Count = %d, want 3", snap.Read.Count)
	}
	if snap.Read.Bytes != 4096+8192+4096 {
		t.Errorf("Read.Bytes = %d, want %d", snap.Read.Bytes, 4096+8192+4096)
	}
	if snap.Read.ErrCount != 1 {
		t.Errorf("Read.ErrCount = %d, want 1", snap.Read.ErrCount)
	}
	if snap.Read.LatMaxNs != 9_000_000 {
		t.Errorf("Read.LatMaxNs = %d, want 9_000_000", snap.Read.LatMaxNs)
	}
	if snap.Read.LatSumNs != 100_000+250_000+9_000_000 {
		t.Errorf("LatSumNs = %d", snap.Read.LatSumNs)
	}
}

func TestStats_BucketBoundaries(t *testing.T) {
	s := NewStats("x", "", 1<<20)
	// Boundary ladder: each sample lands in its named bucket.
	cases := []struct {
		ns      uint64
		wantBkt int // index in latencyBucketsNs (or numLatencyBuckets for overflow)
	}{
		{500, 0},                               // < 1µs → bucket[0] (1µs)
		{1_000, 0},                             // == 1µs → bucket[0]
		{1_001, 1},                             // > 1µs → bucket[1] (2µs)
		{2_000, 1},                             // == 2µs → bucket[1]
		{1_000_000_000, numLatencyBuckets - 1}, // == 1s → last named
		{2_000_000_000, numLatencyBuckets},     // > 1s → overflow
	}
	for _, c := range cases {
		s.Record(BlkTypeIn, 0, c.ns, true)
	}
	snap := s.Snapshot()
	bktCount := func(idx int) uint64 { return snap.Read.LatBuckets[idx] }
	if bktCount(0) != 2 {
		t.Errorf("bucket[0] (≤1µs) = %d, want 2", bktCount(0))
	}
	if bktCount(1) != 2 {
		t.Errorf("bucket[1] (≤2µs) = %d, want 2", bktCount(1))
	}
	if bktCount(numLatencyBuckets-1) != 1 {
		t.Errorf("bucket[1s] = %d, want 1", bktCount(numLatencyBuckets-1))
	}
	if bktCount(numLatencyBuckets) != 1 {
		t.Errorf("overflow bucket = %d, want 1", bktCount(numLatencyBuckets))
	}
}

func TestStats_PercentileEstimate(t *testing.T) {
	s := NewStats("x", "", 1<<20)
	// 90 samples at 100µs (bucket ≤128µs), 10 outliers at 50ms (bucket ≤64ms).
	// With this distribution p50 lands well inside the dense bucket and p99
	// falls in the tail bucket.
	for i := 0; i < 90; i++ {
		s.Record(BlkTypeIn, 0, 100_000, true)
	}
	for i := 0; i < 10; i++ {
		s.Record(BlkTypeIn, 0, 50_000_000, true) // 50ms
	}

	snap := s.Snapshot()
	p50 := snap.Read.P50()
	p99 := snap.Read.P99()
	if p50 < 64_000 || p50 > 128_000 {
		t.Errorf("p50 = %d ns, want 64µs..128µs", p50)
	}
	// p99 must clear the 1ms threshold — true p99 is in the 50ms tail.
	if p99 < 1_000_000 {
		t.Errorf("p99 = %d ns, want >= 1ms (tail bucket)", p99)
	}
}

func TestStats_PercentileEmpty(t *testing.T) {
	s := NewStats("x", "", 1<<20)
	snap := s.Snapshot()
	if snap.Read.P50() != 0 {
		t.Errorf("P50 of empty = %d, want 0", snap.Read.P50())
	}
}

func TestStats_PercentileClampedToMax(t *testing.T) {
	// Single sample at 25µs falls into bucket[5] (≤32µs). Without clamping,
	// p99 interpolates partway into bucket[5] from prevBound=16µs and lands
	// somewhere up to ~32µs, possibly above the actual max of 25µs.
	s := NewStats("x", "", 1<<20)
	s.Record(BlkTypeIn, 0, 25_000, true)
	snap := s.Snapshot().Read
	if got := snap.P99(); got > snap.LatMaxNs {
		t.Errorf("P99 = %d > max %d (must be clamped)", got, snap.LatMaxNs)
	}
}

func TestStats_MarkRead_Coverage(t *testing.T) {
	// 16 KiB backend = 4 blocks of 4 KiB.
	s := NewStats("x", "", 16*1024)
	if s.totalBlocks != 4 {
		t.Fatalf("totalBlocks = %d, want 4", s.totalBlocks)
	}

	// Read first 2 blocks (8 KiB at offset 0).
	s.MarkRead(0, 8192)
	snap := s.Snapshot()
	if snap.LoadedBlocks != 2 {
		t.Errorf("LoadedBlocks after 0..8K = %d, want 2", snap.LoadedBlocks)
	}

	// Re-reading the same range must not double-count.
	s.MarkRead(0, 8192)
	snap = s.Snapshot()
	if snap.LoadedBlocks != 2 {
		t.Errorf("LoadedBlocks after dup read = %d, want 2", snap.LoadedBlocks)
	}

	// Read a partial block: offset 12 KiB, length 1 byte → block 3 covered.
	s.MarkRead(12*1024, 1)
	snap = s.Snapshot()
	if snap.LoadedBlocks != 3 {
		t.Errorf("LoadedBlocks = %d, want 3", snap.LoadedBlocks)
	}

	// Read crossing a block boundary: offset 4095, len 2 → blocks 0 + 1
	// (already covered) — count unchanged.
	s.MarkRead(4095, 2)
	snap = s.Snapshot()
	if snap.LoadedBlocks != 3 {
		t.Errorf("LoadedBlocks after boundary cross = %d, want 3", snap.LoadedBlocks)
	}
}

func TestStats_MarkWrite_Coverage(t *testing.T) {
	s := NewStats("x", "", 8192)
	s.MarkWrite(0, 4096)
	s.MarkWrite(4096, 4096)
	snap := s.Snapshot()
	if snap.WrittenBlocks != 2 {
		t.Errorf("WrittenBlocks = %d, want 2", snap.WrittenBlocks)
	}
	if snap.LoadedBlocks != 0 {
		t.Errorf("LoadedBlocks should be untouched, got %d", snap.LoadedBlocks)
	}
}

func TestStats_MarkBoundsClamped(t *testing.T) {
	s := NewStats("x", "", 4096)
	// Length way past the end — must clamp, not panic.
	s.MarkRead(0, 1<<30)
	snap := s.Snapshot()
	if snap.LoadedBlocks != 1 {
		t.Errorf("LoadedBlocks after over-length read = %d, want 1", snap.LoadedBlocks)
	}
}

func TestStats_NilSafe(t *testing.T) {
	var s *Stats
	// All entry points must tolerate a nil receiver.
	s.Record(BlkTypeIn, 4096, 100, true)
	s.MarkRead(0, 4096)
	s.MarkWrite(0, 4096)
	if got := s.Snapshot(); got.Name != "" {
		t.Errorf("nil Snapshot Name = %q, want empty", got.Name)
	}
}

func TestStats_WriteTo_FormatHasAllSections(t *testing.T) {
	s := NewStats("blk0", "/tmp/img.erofs", 64*1024)
	for i := 0; i < 10; i++ {
		s.Record(BlkTypeIn, 4096, 80_000, true)
	}
	s.MarkRead(0, 16384) // 4 blocks
	s.Record(BlkTypeFlush, 0, 5_000, true)

	var buf bytes.Buffer
	snap := s.Snapshot()
	snap.Extra = map[string]any{"diff_dirty_blocks": 2}
	if _, err := snap.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"backend=blk0",
		"path=/tmp/img.erofs",
		"read",
		"flush",
		"load coverage:",
		"cow  coverage:",
		"backend-extra:",
		"diff_dirty_blocks=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestStats_RequestTypeRouting(t *testing.T) {
	s := NewStats("x", "", 4096)
	s.Record(BlkTypeIn, 100, 1000, true)
	s.Record(BlkTypeOut, 200, 1000, true)
	s.Record(BlkTypeFlush, 0, 1000, true)
	s.Record(BlkTypeDiscard, 0, 1000, true)
	s.Record(BlkTypeWriteZero, 0, 1000, true) // routes to discard bucket

	snap := s.Snapshot()
	if snap.Read.Count != 1 || snap.Write.Count != 1 || snap.Flush.Count != 1 {
		t.Errorf("counts: r=%d w=%d f=%d", snap.Read.Count, snap.Write.Count, snap.Flush.Count)
	}
	if snap.Discard.Count != 2 {
		t.Errorf("Discard.Count = %d, want 2 (Discard + WriteZero)", snap.Discard.Count)
	}
}
