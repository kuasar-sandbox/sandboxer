package vhost

import (
	"fmt"
	"testing"
)

// A test-only uncached synchronous DIO reference, using the same active diff,
// XTS and first-partial-page materialization. This is not a selectable COW mode.
func BenchmarkCOWDirectWorkingSet(b *testing.B) {
	for _, encrypted := range []bool{false, true} {
		for _, work := range []string{"seq-write", "random-4k", "partial-512", "hot-overwrite", "seq-read", "random-read", "multi-disk"} {
			b.Run(fmt.Sprintf("%s/encrypted=%t", work, encrypted), func(b *testing.B) { benchmarkCOWWorkingSet(b, work, encrypted, newDirectWorkingSetCache) })
		}
	}
}
func newDirectWorkingSetCache() (workingSetCache, error) {
	return workingSetCache{
		drain: func() error { return nil }, close: func() error { return nil }, stats: func() map[string]float64 { return nil },
		readAt: func(c *BlockCOW, p []byte, off int64) (int, error) { return c.diff.ReadAt(p, off) },
		writeAt: func(c *BlockCOW, p []byte, off int64) (int, error) {
			if off%cowBlockSize == 0 && len(p)%cowBlockSize == 0 {
				n, err := c.diff.WriteAt(p, off)
				if err == nil {
					for block := off / cowBlockSize; block < (off+int64(n))/cowBlockSize; block++ {
						c.markDirty(block)
					}
				}
				return n, err
			}
			// The workload has 512-byte first writes within a page; retain the exact
			// full-page construction instead of benchmarking sparse-sector writes.
			var page [cowBlockSize]byte
			block := off / cowBlockSize
			if c.blockDirty(block) {
				if err := readFullAt(c.diff, page[:], block*cowBlockSize); err != nil {
					return 0, err
				}
			}
			copy(page[off%cowBlockSize:], p)
			err := writeFullAt(c.diff, page[:], block*cowBlockSize)
			clear(page[:])
			if err != nil {
				return 0, err
			}
			c.markDirty(block)
			return len(p), nil
		},
	}, nil
}
