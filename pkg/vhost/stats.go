package vhost

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// StatsReporter is implemented by Backends or BlockReaders that can
// contribute backend-specific metrics in addition to the generic
// per-request counters and coverage bitmaps tracked by Stats.
//
// The map's keys are short identifiers (e.g. "cow_dirty_blocks") and
// values are typically uint64 / float64 / string. They land in
// StatsSnapshot.Extra and print at the end of the stats summary.
type StatsReporter interface {
	BackendStats() map[string]any
}

// CoverageBlockBytes is the granularity (in bytes) at which the read
// and write coverage bitmaps track which regions of the block device
// have been touched. 4 KiB matches the natural virtio-blk page size.
const CoverageBlockBytes = 4096

// latencyBucketsNs are the upper bounds (inclusive) of the latency
// histogram buckets, covering 1µs..1s exponentially. Samples above 1s
// land in an implicit overflow bucket at index len(latencyBucketsNs).
var latencyBucketsNs = [...]uint64{
	1_000,         // 1µs
	2_000,         // 2µs
	4_000,         // 4µs
	8_000,         // 8µs
	16_000,        // 16µs
	32_000,        // 32µs
	64_000,        // 64µs
	128_000,       // 128µs
	256_000,       // 256µs
	512_000,       // 512µs
	1_000_000,     // 1ms
	2_000_000,     // 2ms
	4_000_000,     // 4ms
	8_000_000,     // 8ms
	16_000_000,    // 16ms
	32_000_000,    // 32ms
	64_000_000,    // 64ms
	128_000_000,   // 128ms
	256_000_000,   // 256ms
	512_000_000,   // 512ms
	1_000_000_000, // 1s
}

const numLatencyBuckets = len(latencyBucketsNs)

// LatencyBucketBoundsNs returns the inclusive upper bounds of the
// latency histogram buckets, in nanoseconds. ReqSnapshot.LatBuckets[i]
// counts samples with latency <= LatencyBucketBoundsNs()[i]; the
// trailing element of LatBuckets (index len(bounds)) is the overflow.
func LatencyBucketBoundsNs() []uint64 {
	out := make([]uint64, len(latencyBucketsNs))
	copy(out, latencyBucketsNs[:])
	return out
}

// ReqStats holds counters for one request type (read / write / flush /
// discard). All fields are accessed atomically; readers should take a
// Snapshot rather than reading the fields directly.
type ReqStats struct {
	count    atomic.Uint64
	bytes    atomic.Uint64
	errCount atomic.Uint64
	latSumNs atomic.Uint64
	latMaxNs atomic.Uint64
	// latBuckets[i] = number of samples with latency <= latencyBucketsNs[i].
	// latBuckets[numLatencyBuckets] = overflow (>1s).
	latBuckets [numLatencyBuckets + 1]atomic.Uint64
}

func (r *ReqStats) record(bytes uint64, latNs uint64, ok bool) {
	r.count.Add(1)
	r.bytes.Add(bytes)
	r.latSumNs.Add(latNs)
	if !ok {
		r.errCount.Add(1)
	}
	for {
		m := r.latMaxNs.Load()
		if latNs <= m {
			break
		}
		if r.latMaxNs.CompareAndSwap(m, latNs) {
			break
		}
	}
	bkt := numLatencyBuckets
	for i, b := range latencyBucketsNs {
		if latNs <= b {
			bkt = i
			break
		}
	}
	r.latBuckets[bkt].Add(1)
}

// Stats aggregates per-request counters plus coverage bitmaps for one
// vhost-user-blk Server.
type Stats struct {
	originMu sync.RWMutex
	name     string
	path     string

	read    ReqStats
	write   ReqStats
	flush   ReqStats
	discard ReqStats

	blockBytes  uint64
	totalBlocks uint64
	readMap     []atomic.Uint64
	writeMap    []atomic.Uint64
}

// NewStats allocates a Stats for a backend with totalBytes capacity.
// name is a short label for printing (e.g. "blk0"); path is best-effort
// origin (file path or URI).
func NewStats(name, path string, totalBytes int64) *Stats {
	if totalBytes < 0 {
		totalBytes = 0
	}
	blockBytes := uint64(CoverageBlockBytes)
	totalBlocks := (uint64(totalBytes) + blockBytes - 1) / blockBytes
	words := (totalBlocks + 63) / 64
	return &Stats{
		name:        name,
		path:        path,
		blockBytes:  blockBytes,
		totalBlocks: totalBlocks,
		readMap:     make([]atomic.Uint64, words),
		writeMap:    make([]atomic.Uint64, words),
	}
}

// Record updates the counters for one completed request.
func (s *Stats) Record(reqType uint32, bytes uint64, latNs uint64, ok bool) {
	if s == nil {
		return
	}
	switch reqType {
	case BlkTypeIn:
		s.read.record(bytes, latNs, ok)
	case BlkTypeOut:
		s.write.record(bytes, latNs, ok)
	case BlkTypeFlush:
		s.flush.record(bytes, latNs, ok)
	case BlkTypeDiscard, BlkTypeWriteZero:
		s.discard.record(bytes, latNs, ok)
	}
}

// MarkRead sets coverage bits for [offset, offset+length).
func (s *Stats) MarkRead(offset int64, length int) {
	if s == nil {
		return
	}
	s.markCoverage(s.readMap, offset, length)
}

// MarkWrite sets write-coverage bits for [offset, offset+length).
func (s *Stats) MarkWrite(offset int64, length int) {
	if s == nil {
		return
	}
	s.markCoverage(s.writeMap, offset, length)
}

func (s *Stats) markCoverage(bitmap []atomic.Uint64, offset int64, length int) {
	if length <= 0 || offset < 0 || s.totalBlocks == 0 {
		return
	}
	blockBytes := int64(s.blockBytes)
	startBlk := uint64(offset / blockBytes)
	endBlk := uint64((offset + int64(length) + blockBytes - 1) / blockBytes)
	if endBlk > s.totalBlocks {
		endBlk = s.totalBlocks
	}
	for blk := startBlk; blk < endBlk; blk++ {
		word := blk / 64
		bit := uint64(1) << (blk % 64)
		for {
			old := bitmap[word].Load()
			if old&bit != 0 {
				break
			}
			if bitmap[word].CompareAndSwap(old, old|bit) {
				break
			}
		}
	}
}

// ReqSnapshot is an immutable view of one ReqStats.
type ReqSnapshot struct {
	Count      uint64
	Bytes      uint64
	ErrCount   uint64
	LatSumNs   uint64
	LatMaxNs   uint64
	LatBuckets [numLatencyBuckets + 1]uint64
}

func (r *ReqStats) snapshot() ReqSnapshot {
	s := ReqSnapshot{
		Count:    r.count.Load(),
		Bytes:    r.bytes.Load(),
		ErrCount: r.errCount.Load(),
		LatSumNs: r.latSumNs.Load(),
		LatMaxNs: r.latMaxNs.Load(),
	}
	for i := range r.latBuckets {
		s.LatBuckets[i] = r.latBuckets[i].Load()
	}
	return s
}

// Percentile estimates the requested percentile (0..1) by linear
// interpolation across the latency histogram buckets, clamped to the
// observed max so estimates can never exceed a sample we actually
// recorded. Returns 0 when no samples have been recorded.
func (r ReqSnapshot) Percentile(p float64) uint64 {
	var total uint64
	for _, v := range r.LatBuckets {
		total += v
	}
	if total == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	} else if p > 1 {
		p = 1
	}
	target := uint64(float64(total) * p)
	if target == 0 {
		target = 1
	}
	var cum, prevBound, result uint64
	for i, v := range r.LatBuckets {
		next := cum + v
		var bound uint64
		if i < numLatencyBuckets {
			bound = latencyBucketsNs[i]
		} else if r.LatMaxNs > 0 {
			bound = r.LatMaxNs
		} else {
			bound = latencyBucketsNs[numLatencyBuckets-1] * 2
		}
		if next >= target {
			if v == 0 {
				result = bound
			} else {
				into := target - cum
				result = prevBound + (bound-prevBound)*into/v
			}
			break
		}
		cum = next
		prevBound = bound
	}
	if r.LatMaxNs > 0 && result > r.LatMaxNs {
		return r.LatMaxNs
	}
	return result
}

func (r ReqSnapshot) P50() uint64 { return r.Percentile(0.50) }
func (r ReqSnapshot) P99() uint64 { return r.Percentile(0.99) }

// Sub returns the per-window delta between this (newer) snapshot and prev:
// counts/bytes/sum/buckets subtract. The window LatMaxNs is exact when a new
// peak was recorded this window (r.LatMaxNs > prev.LatMaxNs); otherwise it
// falls back to the upper bound of the highest non-empty window bucket. Lets a
// periodic reporter show per-interval latency rather than cumulative.
func (r ReqSnapshot) Sub(prev ReqSnapshot) ReqSnapshot {
	w := ReqSnapshot{
		Count:    r.Count - prev.Count,
		Bytes:    r.Bytes - prev.Bytes,
		ErrCount: r.ErrCount - prev.ErrCount,
		LatSumNs: r.LatSumNs - prev.LatSumNs,
	}
	var hi uint64
	for i := range r.LatBuckets {
		w.LatBuckets[i] = r.LatBuckets[i] - prev.LatBuckets[i]
		if w.LatBuckets[i] > 0 {
			if i < numLatencyBuckets {
				hi = latencyBucketsNs[i]
			} else {
				hi = latencyBucketsNs[numLatencyBuckets-1] * 2
			}
		}
	}
	if r.LatMaxNs > prev.LatMaxNs {
		w.LatMaxNs = r.LatMaxNs
	} else {
		w.LatMaxNs = hi
	}
	return w
}

// StatsSnapshot is an immutable view of a Stats.
type StatsSnapshot struct {
	Name          string
	Path          string
	BlockBytes    uint64
	TotalBlocks   uint64
	Read          ReqSnapshot
	Write         ReqSnapshot
	Flush         ReqSnapshot
	Discard       ReqSnapshot
	LoadedBlocks  uint64
	WrittenBlocks uint64
	Extra         map[string]any
}

// Snapshot returns a stable, immutable copy of the current counters.
func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	s.originMu.RLock()
	path := s.path
	s.originMu.RUnlock()
	snap := StatsSnapshot{
		Name:        s.name,
		Path:        path,
		BlockBytes:  s.blockBytes,
		TotalBlocks: s.totalBlocks,
		Read:        s.read.snapshot(),
		Write:       s.write.snapshot(),
		Flush:       s.flush.snapshot(),
		Discard:     s.discard.snapshot(),
	}
	for i := range s.readMap {
		snap.LoadedBlocks += uint64(popcount64(s.readMap[i].Load()))
	}
	for i := range s.writeMap {
		snap.WrittenBlocks += uint64(popcount64(s.writeMap[i].Load()))
	}
	return snap
}

// WriteTo formats snap as a multi-line summary on w.
func (snap StatsSnapshot) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	totalBytes := int64(snap.TotalBlocks * snap.BlockBytes)
	fmt.Fprintf(&buf, "[vhost-stats] backend=%s path=%s size=%s blockSize=%s\n",
		snap.Name, snap.Path, formatBytes(totalBytes), formatBytes(int64(snap.BlockBytes)))

	writeReq := func(label string, r ReqSnapshot) {
		if r.Count == 0 {
			fmt.Fprintf(&buf, "  %-7s reqs=0\n", label)
			return
		}
		fmt.Fprintf(&buf,
			"  %-7s reqs=%-7d bytes=%-10s p50=%-8s p99=%-8s max=%-8s err=%d\n",
			label, r.Count, formatBytes(int64(r.Bytes)),
			formatNs(r.P50()), formatNs(r.P99()), formatNs(r.LatMaxNs), r.ErrCount)
	}
	writeReq("read", snap.Read)
	writeReq("write", snap.Write)
	writeReq("flush", snap.Flush)
	writeReq("discard", snap.Discard)

	if snap.TotalBlocks > 0 {
		loadPct := float64(snap.LoadedBlocks) * 100 / float64(snap.TotalBlocks)
		fmt.Fprintf(&buf,
			"  load coverage:  %d / %d blocks = %.2f%% (%s / %s)\n",
			snap.LoadedBlocks, snap.TotalBlocks, loadPct,
			formatBytes(int64(snap.LoadedBlocks*snap.BlockBytes)),
			formatBytes(totalBytes))
		writePct := float64(snap.WrittenBlocks) * 100 / float64(snap.TotalBlocks)
		fmt.Fprintf(&buf,
			"  cow  coverage:  %d / %d blocks = %.2f%% (%s upper-layer footprint)\n",
			snap.WrittenBlocks, snap.TotalBlocks, writePct,
			formatBytes(int64(snap.WrittenBlocks*snap.BlockBytes)))
	}
	if len(snap.Extra) > 0 {
		keys := make([]string, 0, len(snap.Extra))
		for k := range snap.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(&buf, "  backend-extra:")
		for _, k := range keys {
			fmt.Fprintf(&buf, " %s=%v", k, snap.Extra[k])
		}
		fmt.Fprintln(&buf)
	}
	return buf.WriteTo(w)
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(b) / unit
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func formatNs(ns uint64) string {
	switch {
	case ns < 1_000:
		return fmt.Sprintf("%dns", ns)
	case ns < 1_000_000:
		return fmt.Sprintf("%.1fµs", float64(ns)/1_000)
	case ns < 1_000_000_000:
		return fmt.Sprintf("%.1fms", float64(ns)/1_000_000)
	default:
		return fmt.Sprintf("%.2fs", float64(ns)/1_000_000_000)
	}
}

// nowNs returns the current monotonic-clock nanoseconds. Used by the
// processChain hot path; isolated as a helper so tests can stub it.
var nowNs = func() int64 { return time.Now().UnixNano() }

// SetOrigin updates binding provenance without resetting pre-launch counters.
func (s *Stats) SetOrigin(path string) {
	if s == nil {
		return
	}
	s.originMu.Lock()
	s.path = path
	s.originMu.Unlock()
}
