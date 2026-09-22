package vhost

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type measuredBase struct {
	io.ReaderAt
	size         int64
	calls, bytes uint64
}

func (r *measuredBase) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	n, err := r.ReaderAt.ReadAt(p, off)
	r.bytes += uint64(n)
	return n, err
}
func (r *measuredBase) Size() int64 { return r.size }
func (*measuredBase) Close() error  { return nil }

// Buffered-file call counts are actual file ReadAt calls, not a claim of device
// reads: the immutable fixture remains naturally warm in the kernel page cache.
func BenchmarkCOWBaseRange(b *testing.B) {
	for _, file := range []bool{false, true} {
		for _, size := range []int{cowBlockSize, maxDiffScratchSize} {
			b.Run(fmt.Sprintf("file=%t/bytes=%d", file, size), func(b *testing.B) {
				base := &measuredBase{ReaderAt: zeroBlockReader{size: maxDiffScratchSize}, size: maxDiffScratchSize}
				if file {
					path := filepath.Join(b.TempDir(), "base")
					if err := os.WriteFile(path, make([]byte, maxDiffScratchSize), 0600); err != nil {
						b.Fatal(err)
					}
					f, err := os.Open(path)
					if err != nil {
						b.Fatal(err)
					}
					defer f.Close()
					base.ReaderAt = f
				}
				cow, err := OpenBlockCOW(filepath.Join(b.TempDir(), "diff"), base, DiffInit{CreateSize: maxDiffScratchSize})
				if err != nil {
					b.Fatal(err)
				}
				defer cow.Close()
				buf := make([]byte, size)
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if n, err := cow.ReadAt(buf, 0); err != nil || n != size {
						b.Fatalf("read %d: %v", n, err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(base.calls)/float64(b.N), "base-calls/op")
				b.ReportMetric(float64(base.bytes)/float64(b.N), "base-B/op")
			})
		}
	}
}
