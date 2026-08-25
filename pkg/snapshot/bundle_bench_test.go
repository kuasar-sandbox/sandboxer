package snapshot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
)

// BenchmarkSnapshotSinkCreate compares the two local snapshot formats over
// one memory layer and one disk layer. The inputs share 4 MiB to exercise the
// Bundle's cross-Manifest object dedup without making the whole fixture
// artificially compressible.
func BenchmarkSnapshotSinkCreate(b *testing.B) {
	const layerSize = 16 << 20
	disk := benchmarkBytes(layerSize, 0x1234_5678_9abc_def0)
	memory := benchmarkBytes(layerSize, 0xfedc_ba98_7654_3210)
	copy(memory[:4<<20], disk[:4<<20])
	inner, err := BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		b.Fatal(err)
	}
	customerKey := [32]byte{0x91, 0x92, 0x93}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "512KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}

	for _, mode := range []string{"local-tarstream", "manifest-bundle"} {
		b.Run(mode, func(b *testing.B) {
			base := b.TempDir()
			var outputBytes uint64
			b.SetBytes(2 * layerSize)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dir := filepath.Join(base, strconv.Itoa(i))
				if err := os.Mkdir(dir, 0o755); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				switch mode {
				case "local-tarstream":
					sink := NewFileSink(dir, "bench", nil, false, nil)
					if _, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(disk), nil); err != nil {
						b.Fatal(err)
					}
					if _, _, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(memory), nil, bytes.NewReader(inner)); err != nil {
						b.Fatal(err)
					}
				case "manifest-bundle":
					sink, err := NewBundleSink(context.Background(), dir, "bench", cfg,
						func() ([32]byte, error) { return customerKey, nil }, nil)
					if err != nil {
						b.Fatal(err)
					}
					if _, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(disk), nil); err != nil {
						_ = sink.Close()
						b.Fatal(err)
					}
					if _, _, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(memory), nil, bytes.NewReader(inner)); err != nil {
						_ = sink.Close()
						b.Fatal(err)
					}
					if err := sink.Close(); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				outputBytes += benchmarkDirectoryBytes(b, dir)
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.StopTimer()
			if b.N > 0 {
				b.ReportMetric(float64(outputBytes)/float64(b.N), "artifact-B/op")
			}
		})
	}
}

func benchmarkBytes(size int, seed uint64) []byte {
	result := make([]byte, size)
	x := seed
	for i := range result {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		result[i] = byte(x)
	}
	return result
}

func benchmarkDirectoryBytes(b *testing.B, directory string) uint64 {
	b.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		b.Fatal(err)
	}
	var total uint64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			b.Fatal(err)
		}
		if info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
	}
	return total
}
