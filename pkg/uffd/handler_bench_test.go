package uffd

import (
	"hash/fnv"
	"testing"
)

// pageIdxHashFNVReference is the FNV-1a implementation pageIdxHash used
// before the splitmix64 finalizer, kept as the benchmark baseline. It must
// stay behaviorally identical to the code it mirrors: fnv-1a over the 8
// little-endian bytes of the page index.
func pageIdxHashFNVReference(idx uint64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for i := 0; i < 8; i++ {
		b[i] = byte(idx >> (i * 8))
	}
	_, _ = h.Write(b[:])
	return h.Sum64()
}

var pageIdxHashSink uint64

// BenchmarkPageIdxHash measures per-fault hashing cost. Both
// implementations are called directly, the way dispatch() calls
// pageIdxHash, so the splitmix64 number reflects the inlined call site
// rather than a call through a function value. pageIdxHash runs once per
// pagefault, before the worker-queue send: latency and allocations both
// matter here.
func BenchmarkPageIdxHash(b *testing.B) {
	b.Run("splitmix64", func(b *testing.B) {
		var sink uint64
		for i := 0; i < b.N; i++ {
			sink += pageIdxHash(uint64(i))
		}
		pageIdxHashSink += sink
	})
	b.Run("fnv64a", func(b *testing.B) {
		var sink uint64
		for i := 0; i < b.N; i++ {
			sink += pageIdxHashFNVReference(uint64(i))
		}
		pageIdxHashSink += sink
	})
}
