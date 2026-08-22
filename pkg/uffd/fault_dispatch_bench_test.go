package uffd

import "testing"

// BenchmarkPageIdxHash tracks the fixed per-fault cost of worker routing.
func BenchmarkPageIdxHash(b *testing.B) {
	var sink uint64
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		sink ^= pageIdxHash(uint64(i))
	}

	benchHashSink = sink
}

var benchHashSink uint64
