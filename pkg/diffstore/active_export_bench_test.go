package diffstore

import (
	"context"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// These benchmarks isolate metadata work without creating a large image file.
// The fixture and measured APIs also work before the export optimizations.
func benchmarkExportDisk(size int64, word uint64) *Disk {
	bitmap := make([]uint64, (size/cowBlockSize+63)/64)
	for i := range bitmap {
		bitmap[i] = word
	}
	return &Disk{
		size: size, bitmap: bitmap, lifeCtx: context.Background(),
		cache: &Cache{}, diff: &diffFile{logicalSize: size},
	}
}

func BenchmarkActiveExportMetadata(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int64
		word uint64
	}{
		{"32GiBAlternating", 32 << 30, 0xaaaaaaaaaaaaaaaa},
		{"1TiBHole", 1 << 40, 0},
		{"1TiBPresent", 1 << 40, ^uint64(0)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			cow := benchmarkExportDisk(tc.size, tc.word)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				view, err := cow.Export(context.Background(), ActiveDiff)
				if err != nil {
					b.Fatal(err)
				}
				if err := view.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkActiveSourceLongHole(b *testing.B) {
	const size = 1 << 40
	cow := benchmarkExportDisk(size, 0)
	view, err := cow.Export(context.Background(), ActiveDiff)
	if err != nil {
		b.Fatal(err)
	}
	defer view.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		run, err := view.RunAt(17, size-17)
		if err != nil {
			b.Fatal(err)
		}
		if run.Offset() != 17 || run.End() != size || run.Kind() != sparse.Hole {
			b.Fatalf("unexpected run: [%d,%d) kind=%v", run.Offset(), run.End(), run.Kind())
		}
	}
}
