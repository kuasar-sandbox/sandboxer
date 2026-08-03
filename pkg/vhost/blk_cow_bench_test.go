package vhost

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const benchmarkCOWBlocks = 256

type zeroBlockReader struct{ size int64 }

func (r zeroBlockReader) ReadAt(buf []byte, offset int64) (int, error) {
	clear(buf)
	return len(buf), nil
}

func (r zeroBlockReader) Size() int64 { return r.size }
func (zeroBlockReader) Close() error  { return nil }

func BenchmarkBlockCOWWriteAt(b *testing.B) {
	const size = benchmarkCOWBlocks * cowBlockSize
	open := func(b *testing.B) *BlockCOW {
		b.Helper()
		cow, err := OpenBlockCOW(
			filepath.Join(b.TempDir(), "diff.ext4"),
			zeroBlockReader{size: size},
			size,
		)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = cow.Close() })
		return cow
	}

	b.Run("clean-partial-512", func(b *testing.B) {
		cow := open(b)
		payload := make([]byte, 512)
		b.SetBytes(int64(len(payload)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if i > 0 && i%benchmarkCOWBlocks == 0 {
				b.StopTimer()
				resetBenchmarkCOW(b, cow)
				b.StartTimer()
			}
			offset := int64(i%benchmarkCOWBlocks)*cowBlockSize + 123
			if _, err := cow.WriteAt(payload, offset); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("clean-full-4k", func(b *testing.B) {
		cow := open(b)
		payload := make([]byte, cowBlockSize)
		b.SetBytes(int64(len(payload)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if i > 0 && i%benchmarkCOWBlocks == 0 {
				b.StopTimer()
				resetBenchmarkCOW(b, cow)
				b.StartTimer()
			}
			offset := int64(i%benchmarkCOWBlocks) * cowBlockSize
			if _, err := cow.WriteAt(payload, offset); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("dirty-partial-512", func(b *testing.B) {
		cow := open(b)
		if _, err := cow.WriteAt(make([]byte, cowBlockSize), 0); err != nil {
			b.Fatal(err)
		}
		payload := make([]byte, 512)
		b.SetBytes(int64(len(payload)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := cow.WriteAt(payload, 123); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func resetBenchmarkCOW(b *testing.B, cow *BlockCOW) {
	b.Helper()
	if err := unix.Fallocate(
		int(cow.diff.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		0,
		cow.size,
	); err != nil {
		b.Fatal(err)
	}
	cow.bitmapMu.Lock()
	clear(cow.bitmap)
	cow.bitmapMu.Unlock()
}
