package uffd

import (
	"fmt"
	"runtime"
	"testing"
)

func expectedFallback() int {
	c := runtime.NumCPU()
	if c < MinWorkers {
		return MinWorkers
	}
	return c
}

func TestWorkerCount(t *testing.T) {
	tests := []struct {
		name       string
		numWorkers int
		want       int
	}{
		{name: "explicit worker count wins", numWorkers: 8, want: 8},
		{name: "uses runtime cpu count when unset", numWorkers: 0, want: expectedFallback()},
		{name: "enforces minimum for explicit small count", numWorkers: 1, want: MinWorkers},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workerCount(tt.numWorkers); got != tt.want {
				t.Errorf("workerCount(%d) = %d, want %d", tt.numWorkers, got, tt.want)
			}
		})
	}
}

// TestPageIdxHashDistribution guards the property dispatch relies on:
// hashed page indices spread evenly across worker queues, so page-in
// storms never bottleneck on one worker. The grid keeps one
// representative per failure class for a multiply-xorshift mixer —
// sequential (dense low bits) and power-of-two stride (THP tail faults,
// one page per 2MiB) access, against prime, power-of-two, and
// production-sized worker counts, including runtime.NumCPU() itself
// (workerCount()'s uncapped default).
func TestPageIdxHashDistribution(t *testing.T) {
	patterns := []struct {
		name   string
		stride uint64
	}{
		{"sequential", 1},
		{"stride-512", 512},
	}
	workerCounts := []int{3, 64, 88, runtime.NumCPU()}
	for _, p := range patterns {
		seen := make(map[int]bool)
		for _, workers := range workerCounts {
			if seen[workers] {
				continue
			}
			seen[workers] = true
			name := fmt.Sprintf("%s/%d-workers", p.name, workers)
			t.Run(name, func(t *testing.T) {
				assertQueueSpread(t, name, p.stride, workers)
			})
		}
	}
}

// assertQueueSpread hashes pages indices of the given stride into workers
// buckets and fails when the busiest bucket exceeds 1.1x the mean.
func assertQueueSpread(t *testing.T, name string, stride uint64, workers int) {
	t.Helper()
	const pages = 1 << 20
	counts := make([]uint64, workers)
	for i := uint64(0); i < pages; i++ {
		counts[pageIdxHash(i*stride)%uint64(workers)]++
	}
	var max uint64
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	if got := float64(max) / (float64(pages) / float64(workers)); got > 1.1 {
		t.Errorf("%s: busiest queue holds %.3fx the mean, want <= 1.1", name, got)
	}
}
