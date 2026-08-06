package uffd

import "sync/atomic"

// latencyBucketsNs are the inclusive upper bounds of the page-in latency
// histogram buckets, covering 1µs..1s exponentially; samples above 1s land in
// an implicit overflow bucket at index len(latencyBucketsNs). The scheme
// mirrors pkg/vhost so uffd page-in and vhost block latency read the same way.
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

// latHist is a lock-free latency histogram (count + sum + max + buckets),
// safe for concurrent record from every fault worker.
type latHist struct {
	count   atomic.Uint64
	sumNs   atomic.Uint64
	maxNs   atomic.Uint64
	buckets [numLatencyBuckets + 1]atomic.Uint64 // [n] = overflow (>1s)
}

func (h *latHist) record(latNs uint64) {
	h.count.Add(1)
	h.sumNs.Add(latNs)
	for {
		m := h.maxNs.Load()
		if latNs <= m {
			break
		}
		if h.maxNs.CompareAndSwap(m, latNs) {
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
	h.buckets[bkt].Add(1)
}

// LatSnapshot is an immutable view of a latency histogram (cumulative when
// taken from a latHist; a windowed delta when produced by Sub).
type LatSnapshot struct {
	Count   uint64
	SumNs   uint64
	MaxNs   uint64
	Buckets [numLatencyBuckets + 1]uint64
}

func (h *latHist) snapshot() LatSnapshot {
	s := LatSnapshot{Count: h.count.Load(), SumNs: h.sumNs.Load(), MaxNs: h.maxNs.Load()}
	for i := range h.buckets {
		s.Buckets[i] = h.buckets[i].Load()
	}
	return s
}

// Sub returns the per-window delta between this (newer) snapshot and prev: the
// counts/sum/buckets subtract. The window MaxNs is exact when a new peak was
// recorded this window (s.MaxNs > prev.MaxNs); otherwise it falls back to the
// upper bound of the highest non-empty window bucket (the cumulative max can't
// be subtracted). This makes each periodic report cover only the last interval.
func (s LatSnapshot) Sub(prev LatSnapshot) LatSnapshot {
	w := LatSnapshot{Count: s.Count - prev.Count, SumNs: s.SumNs - prev.SumNs}
	var hi uint64
	for i := range s.Buckets {
		w.Buckets[i] = s.Buckets[i] - prev.Buckets[i]
		if w.Buckets[i] > 0 {
			if i < numLatencyBuckets {
				hi = latencyBucketsNs[i]
			} else {
				hi = latencyBucketsNs[numLatencyBuckets-1] * 2
			}
		}
	}
	if s.MaxNs > prev.MaxNs {
		w.MaxNs = s.MaxNs // a new peak occurred this window — exact
	} else {
		w.MaxNs = hi // no new peak; coarse (bucket upper bound)
	}
	return w
}

// Percentile estimates the requested percentile (0..1) by linear interpolation
// across the buckets, clamped to the observed max. Returns 0 with no samples.
// Same algorithm as pkg/vhost ReqSnapshot.Percentile.
func (s LatSnapshot) Percentile(p float64) uint64 {
	var total uint64
	for _, v := range s.Buckets {
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
	for i, v := range s.Buckets {
		next := cum + v
		var bound uint64
		if i < numLatencyBuckets {
			bound = latencyBucketsNs[i]
		} else if s.MaxNs > 0 {
			bound = s.MaxNs
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
	if s.MaxNs > 0 && result > s.MaxNs {
		return s.MaxNs
	}
	return result
}

func (s LatSnapshot) P50() uint64 { return s.Percentile(0.50) }
func (s LatSnapshot) P95() uint64 { return s.Percentile(0.95) }
func (s LatSnapshot) P99() uint64 { return s.Percentile(0.99) }
