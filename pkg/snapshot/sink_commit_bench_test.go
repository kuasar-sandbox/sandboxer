package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

const fileSinkCommitBenchmarkSize = 256 << 20

type benchmarkZeroReaderAt struct{}

func (benchmarkZeroReaderAt) ReadAt(buffer []byte, _ int64) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

// BenchmarkFileSinkFreshCommit256MiB guards the local-output performance
// boundary. FileSink writes one same-directory temporary artifact and commits
// it with the existing O(1) atomic rename; named-location publication must not
// route this path through its shared-final copy protocol.
func BenchmarkFileSinkFreshCommit256MiB(b *testing.B) {
	source, err := sparse.NewSource(benchmarkZeroReaderAt{}, fileSinkCommitBenchmarkSize, nil)
	if err != nil {
		b.Fatal(err)
	}
	base := b.TempDir()
	var artifactBytes uint64
	b.SetBytes(fileSinkCommitBenchmarkSize)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		directory := filepath.Join(base, strconv.Itoa(i))
		if err := os.Mkdir(directory, 0o755); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		_, path, err := NewFileSink(directory, "benchmark", nil, false, nil).
			AbsorbOverlaySource(context.Background(), source)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		info, err := os.Stat(path)
		if err != nil {
			b.Fatal(err)
		}
		artifactBytes += uint64(info.Size())
		entries, err := os.ReadDir(directory)
		if err != nil {
			b.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
			b.Fatalf("local FileSink entries = %v, want only %q", entries, filepath.Base(path))
		}
		if err := os.RemoveAll(directory); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.StopTimer()
	if b.N > 0 {
		b.ReportMetric(float64(artifactBytes)/float64(b.N), "artifact-B/op")
	}
}
