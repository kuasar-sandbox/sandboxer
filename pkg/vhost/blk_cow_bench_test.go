package vhost

import (
	"io"
	"path/filepath"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
)

const benchmarkCOWBlocks = 256

type zeroBlockReader struct{ size int64 }

func (r zeroBlockReader) ReadAt(buf []byte, offset int64) (int, error) {
	clear(buf)
	return len(buf), nil
}

func BenchmarkEncryptedBlockCOW(b *testing.B) {
	const size = benchmarkCOWBlocks * cowBlockSize
	codec, err := manifestcrypto.NewTarStreamCodec([32]byte{0x92})
	if err != nil {
		b.Fatal(err)
	}
	open := func(b *testing.B) *BlockCOW {
		b.Helper()
		cow, err := OpenBlockCOW(
			filepath.Join(b.TempDir(), "diff.ext4"),
			zeroBlockReader{size: size},
			DiffInit{CreateSize: size},
			WithCodec(codec, false),
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
		b.ReportAllocs()
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

	for _, test := range []struct {
		name   string
		offset int64
		size   int
	}{
		{name: "dirty-sector-512", offset: 512, size: 512},
		{name: "dirty-partial-257", offset: 123, size: 257},
		{name: "dirty-full-4k", offset: 0, size: cowBlockSize},
	} {
		b.Run(test.name, func(b *testing.B) {
			cow := open(b)
			if _, err := cow.WriteAt(make([]byte, cowBlockSize), 0); err != nil {
				b.Fatal(err)
			}
			payload := make([]byte, test.size)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := cow.WriteAt(payload, test.offset); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

	for _, test := range []struct {
		name string
		size int
	}{
		{name: "read-dirty-512", size: 512},
		{name: "read-dirty-4k", size: cowBlockSize},
	} {
		b.Run(test.name, func(b *testing.B) {
			cow := open(b)
			if _, err := cow.WriteAt(make([]byte, cowBlockSize), 0); err != nil {
				b.Fatal(err)
			}
			buffer := make([]byte, test.size)
			b.SetBytes(int64(len(buffer)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := cow.ReadAt(buffer, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
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
			DiffInit{CreateSize: size},
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

func BenchmarkBlockCOWSnapshotView(b *testing.B) {
	const size = benchmarkCOWBlocks * cowBlockSize
	cow, err := OpenBlockCOW(
		filepath.Join(b.TempDir(), "diff.ext4"),
		zeroBlockReader{size: size},
		DiffInit{CreateSize: size},
	)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cow.Close() })
	block := make([]byte, cowBlockSize)
	for i := int64(0); i < benchmarkCOWBlocks; i += 2 {
		if _, err := cow.WriteAt(block, i*cowBlockSize); err != nil {
			b.Fatal(err)
		}
	}

	b.Run("create", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			view, holes, err := cow.SnapshotView()
			if err != nil || view == nil || len(holes) == 0 {
				b.Fatalf("SnapshotView: view=%v holes=%d err=%v", view, len(holes), err)
			}
		}
	})

	benchmarkRead := func(b *testing.B, offset int64) {
		view, _, err := cow.SnapshotView()
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, cowBlockSize)
		b.SetBytes(cowBlockSize)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := view.Seek(offset, io.SeekStart); err != nil {
				b.Fatal(err)
			}
			if _, err := io.ReadFull(view, buf); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("read-dirty-4k", func(b *testing.B) { benchmarkRead(b, 0) })
	b.Run("read-clean-4k", func(b *testing.B) { benchmarkRead(b, cowBlockSize) })

	b.Run("read-dense-1m", func(b *testing.B) {
		dense, err := OpenBlockCOW(
			filepath.Join(b.TempDir(), "dense.ext4"),
			zeroBlockReader{size: size},
			DiffInit{CreateSize: size},
		)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = dense.Close() })
		if _, err := dense.WriteAt(make([]byte, size), 0); err != nil {
			b.Fatal(err)
		}
		view, _, err := dense.SnapshotView()
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, size)
		b.SetBytes(size)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := view.Seek(0, io.SeekStart); err != nil {
				b.Fatal(err)
			}
			if _, err := io.ReadFull(view, buf); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func resetBenchmarkCOW(b *testing.B, cow *BlockCOW) {
	b.Helper()
	if err := cow.diff.punchHole(0, cow.size); err != nil {
		b.Fatal(err)
	}
	cow.bitmapMu.Lock()
	clear(cow.bitmap)
	cow.bitmapMu.Unlock()
}
