package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

// BenchmarkManifestBundleUpload measures the complete upload-snapshot path:
// strict root reading, recorded-generation admission over Unix gRPC, physical
// object verification, key-table unsealing, salt-domain validation, and exact
// object Put calls. RejectSaltDomain uses byte-identical physical objects with
// a forged canonical admission from another generation.
func BenchmarkManifestBundleUpload(b *testing.B) {
	b.StopTimer()
	const layerSize = 16 << 20
	customerKey := [32]byte{0xc1, 0xc2, 0xc3}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	cfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "512KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	directory := b.TempDir()
	sink, err := snapshot.NewBundleSink(context.Background(), directory, "upload-bench", cfg, keyFn, nil)
	if err != nil {
		b.Fatal(err)
	}
	disk := bundleUploadBenchmarkBytes(layerSize, 0x1234_5678_9abc_def0)
	memory := bundleUploadBenchmarkBytes(layerSize, 0xfedc_ba98_7654_3210)
	copy(memory[:4<<20], disk[:4<<20])
	diskRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(disk), nil)
	if err != nil {
		b.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {},
		"snapshot.cfg": []byte("boot:\n  root:\n    base: " + diskRef + "\n"),
	})
	if err != nil {
		b.Fatal(err)
	}
	rootRef, sourcePath, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(memory), nil, bytes.NewReader(inner))
	if err != nil {
		b.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		b.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		b.Fatal(err)
	}

	g2Salt, err := store.SaltForGeneration("G2")
	if err != nil {
		b.Fatal(err)
	}
	forgedDir := filepath.Join(directory, "forged")
	if err := os.Mkdir(forgedDir, 0o755); err != nil {
		b.Fatal(err)
	}
	forgedPath := filepath.Join(forgedDir, manifest.HexKey(root)+".bundle")
	copyBundleObjectsWithAdmission(b, sourcePath, forgedPath, root,
		store.WriteAdmission{Generation: "G2", Salt: g2Salt})
	socket, _ := startBundleUploadStore(b, []store.Generation{"G1", "G2"})
	uploadCfg := *cfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket, Pool: 4, Timeout: "30s"}

	for _, benchmark := range []struct {
		name      string
		path      string
		wantError string
	}{
		{name: "ExactUpload", path: sourcePath},
		{name: "RejectSaltDomain", path: forgedPath, wantError: "salt domain"},
	} {
		benchmark := benchmark
		b.Run(benchmark.name, func(b *testing.B) {
			if benchmark.wantError == "" {
				b.SetBytes(info.Size())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := UploadLocal(context.Background(), benchmark.path, &uploadCfg,
					keyFn, nil, nil, false, nil)
				if benchmark.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), benchmark.wantError) {
						b.Fatalf("UploadLocal error = %v, want %q", err, benchmark.wantError)
					}
					continue
				}
				if err != nil {
					b.Fatal(err)
				}
				if got != rootRef {
					b.Fatalf("root = %q, want %q", got, rootRef)
				}
			}
		})
	}

	b.StopTimer()
	multi := writeMultiSourceUploadFixture(b)
	multiSocket, _ := startBundleUploadStore(b, []store.Generation{"G1", "G2", "G3"})
	multiCfg := *multi.currentCfg
	multiCfg.Store = manifest.StoreConfig{Endpoint: multiSocket, Pool: 4, Timeout: "30s"}
	var multiBytes int64
	for _, path := range []string{multi.sourceAPath, multi.sourceBPath, multi.currentPath} {
		info, err := os.Stat(path)
		if err != nil {
			b.Fatal(err)
		}
		multiBytes += info.Size()
	}
	b.Run("ThreeBundleSources", func(b *testing.B) {
		b.SetBytes(multiBytes)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			got, err := UploadLocalWithLocations(context.Background(), multi.currentPath, &multiCfg,
				func() ([32]byte, error) { return multi.customerKey, nil }, nil, multi.locations, nil, false, nil)
			if err != nil {
				b.Fatal(err)
			}
			if want := "manifest://" + manifest.HexKey(multi.currentRoot); got != want {
				b.Fatalf("root = %q, want %q", got, want)
			}
		}
	})
}

func bundleUploadBenchmarkBytes(size int, seed uint64) []byte {
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
