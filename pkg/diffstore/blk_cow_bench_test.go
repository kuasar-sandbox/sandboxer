package diffstore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const benchmarkCOWBlocks = 256

type zeroBlockReader struct{ size int64 }

func (r zeroBlockReader) ReadAt(buf []byte, offset int64) (int, error) {
	clear(buf)
	return len(buf), nil
}

func BenchmarkEncryptedBlockCOW(b *testing.B) {
	const size = benchmarkCOWBlocks * cowBlockSize
	key := [32]byte{0x92}
	open := func(b *testing.B) *benchmarkCOW {
		return newBenchmarkCOW(b, size, WithDiffEncryption(key, false))
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
				cow.recreate()
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
	open := func(b *testing.B) *benchmarkCOW {
		return newBenchmarkCOW(b, size)
	}

	b.Run("clean-partial-512", func(b *testing.B) {
		cow := open(b)
		payload := make([]byte, 512)
		b.SetBytes(int64(len(payload)))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if i > 0 && i%benchmarkCOWBlocks == 0 {
				b.StopTimer()
				cow.recreate()
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
				cow.recreate()
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
	cow, err := OpenDisk(
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
		dense, err := OpenDisk(
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

// A clean-write benchmark must start from a new Hole-backed DIFF. Recreating
// the same path outside the timer retains one fixture directory and cleanup,
// rather than turning the next iteration into a write over discarded Zero.
type benchmarkCOW struct {
	*Disk
	recreate func()
}

func newBenchmarkCOW(b *testing.B, size int64, options ...DiskOption) *benchmarkCOW {
	b.Helper()
	path := filepath.Join(b.TempDir(), "diff.ext4")
	base := zeroBlockReader{size: size}
	fixture := &benchmarkCOW{}
	fixture.recreate = func() {
		b.Helper()
		if fixture.Disk != nil {
			if err := fixture.Close(); err != nil {
				b.Fatal(err)
			}
			fixture.Disk = nil
			if err := os.Remove(path); err != nil {
				b.Fatal(err)
			}
		}
		var err error
		fixture.Disk, err = OpenDisk(path, base, DiffInit{CreateSize: size}, options...)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Cleanup(func() {
		if fixture.Disk != nil {
			if err := fixture.Close(); err != nil {
				b.Error(err)
			}
		}
	})
	fixture.recreate()
	return fixture
}

func BenchmarkFreshTemplateMaterialization(b *testing.B) {
	const size = 256 << 20
	templateDir := b.TempDir()
	templatePath := filepath.Join(templateDir, "template.ext4")
	f, err := os.OpenFile(templatePath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		b.Fatal(err)
	}
	block := make([]byte, cowBlockSize)
	for off := int64(0); off < size; off += 1 << 20 {
		block[0] = byte(off >> 20)
		if _, err := f.WriteAt(block, off); err != nil {
			b.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}

	b.Run("open", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			path := filepath.Join(b.TempDir(), "active.diff")
			b.StartTimer()
			cow, err := OpenDisk(path, nil, DiffInit{Template: rawTemplateFactory(templatePath)}, WithDiscardOnClose())
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			if err := cow.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("complete", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			path := filepath.Join(b.TempDir(), "active.diff")
			cow, err := OpenDisk(path, nil, DiffInit{Template: rawTemplateFactory(templatePath)})
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			err = cow.WaitMaterialized(context.Background())
			b.StopTimer()
			if err != nil {
				b.Fatal(err)
			}
			if err := cow.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}
