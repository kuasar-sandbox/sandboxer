package uffd

import (
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
