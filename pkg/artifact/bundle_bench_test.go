package artifact_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

// BenchmarkBundleReadAt exercises ordinary Bundle-backed Fetcher reads with
// both verification policies. SameChunk guards plaintext-cache reuse; Random
// crosses physical Chunk boundaries; Sequential models disk-style reads.
func BenchmarkBundleReadAt(b *testing.B) {
	b.StopTimer()
	const imageSize = 32 << 20
	payload := bundleBenchmarkBytes(imageSize)
	customerKey := [32]byte{0xa1, 0xa2, 0xa3}
	baseCfg := manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "512KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	directory := b.TempDir()
	sink, err := snapshot.NewBundleSink(context.Background(), directory, "read-bench", &baseCfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		b.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		b.Fatal(err)
	}
	_, path, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(payload), nil, bytes.NewReader(inner))
	if err != nil {
		_ = sink.Close()
		b.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		b.Fatal(err)
	}

	patterns := []struct {
		name string
		size int
		off  func(int) uint64
	}{
		{name: "SameChunk4KiB", size: 4 << 10, off: func(i int) uint64 { return uint64(i%128) * 4096 }},
		{name: "RandomChunk4KiB", size: 4 << 10, off: func(i int) uint64 {
			return uint64((uint64(i)*104729)%uint64(imageSize/(4<<10))) * (4 << 10)
		}},
		{name: "Sequential1MiB", size: 1 << 20, off: func(i int) uint64 {
			return uint64(i%(imageSize/(1<<20))) * (1 << 20)
		}},
	}
	for _, verify := range []bool{true, false} {
		verify := verify
		policy := "VerifyTrue"
		if !verify {
			policy = "VerifyFalse"
		}
		for _, pattern := range patterns {
			pattern := pattern
			b.Run(policy+"/"+pattern.name, func(b *testing.B) {
				cfg := baseCfg
				cfg.Manifest.VerifyContent = &verify
				opened, err := artifact.OpenFile(context.Background(), path,
					manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}, &cfg,
					func() ([32]byte, error) { return customerKey, nil }, nil, nil, false)
				if err != nil {
					b.Fatal(err)
				}
				defer opened.Close()
				buffer := make([]byte, pattern.size)
				b.SetBytes(int64(pattern.size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					offset := pattern.off(i)
					n, err := opened.ReadAt(context.Background(), buffer, offset)
					if err != nil || n != len(buffer) {
						b.Fatalf("ReadAt(offset=%d)=%d, %v", offset, n, err)
					}
				}
			})
		}
	}
}

// BenchmarkSnapshotArtifactReadLatencyQuantiles compares the ordinary local
// restore read path for the legacy tarstream and the new Bundle. The Bundle
// uses the default strict verification policy; random 4 KiB reads exercise
// cross-Chunk faults and sequential 1 MiB reads model disk traversal.
func BenchmarkSnapshotArtifactReadLatencyQuantiles(b *testing.B) {
	b.StopTimer()
	const imageSize = 32 << 20
	payload := bundleBenchmarkBytes(imageSize)
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		b.Fatal(err)
	}
	customerKey := [32]byte{0xb1, 0xb2, 0xb3}
	verify := true
	cfg := manifest.Config{
		Manifest: manifest.ManifestSubConfig{VerifyContent: &verify},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "512KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}

	tarDir := b.TempDir()
	_, tarPath, err := snapshot.NewFileSink(tarDir, "latency-tar", nil, false, nil).AbsorbBundle(
		context.Background(), bytes.NewReader(payload), nil, bytes.NewReader(inner))
	if err != nil {
		b.Fatal(err)
	}
	bundleDir := b.TempDir()
	bundleSink, err := snapshot.NewBundleSink(context.Background(), bundleDir, "latency-bundle", &cfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		b.Fatal(err)
	}
	_, bundlePath, err := bundleSink.AbsorbBundle(context.Background(), bytes.NewReader(payload), nil, bytes.NewReader(inner))
	if err != nil {
		_ = bundleSink.Close()
		b.Fatal(err)
	}
	if err := bundleSink.Close(); err != nil {
		b.Fatal(err)
	}

	artifacts := []struct {
		name string
		path string
	}{
		{name: "Tarstream", path: tarPath},
		{name: "BundleVerifyTrue", path: bundlePath},
	}
	patterns := []struct {
		name string
		size int
		off  func(int) uint64
	}{
		{name: "Random4KiB", size: 4 << 10, off: func(i int) uint64 {
			return uint64((uint64(i)*104729)%uint64(imageSize/(4<<10))) * (4 << 10)
		}},
		{name: "Sequential1MiB", size: 1 << 20, off: func(i int) uint64 {
			return uint64(i%(imageSize/(1<<20))) * (1 << 20)
		}},
	}
	for _, artifactCase := range artifacts {
		artifactCase := artifactCase
		for _, pattern := range patterns {
			pattern := pattern
			b.Run(artifactCase.name+"/"+pattern.name, func(b *testing.B) {
				opened, err := artifact.OpenFile(context.Background(), artifactCase.path,
					manifest.Ref{Scheme: manifest.RefSchemeFile, Path: artifactCase.path}, &cfg,
					func() ([32]byte, error) { return customerKey, nil }, nil, nil, false)
				if err != nil {
					b.Fatal(err)
				}
				defer opened.Close()
				buffer := make([]byte, pattern.size)
				samples := make([]int64, b.N)
				b.SetBytes(int64(pattern.size))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					started := time.Now()
					n, err := opened.ReadAt(context.Background(), buffer, pattern.off(i))
					if err != nil || n != len(buffer) {
						b.Fatalf("ReadAt = %d, %v", n, err)
					}
					samples[i] = time.Since(started).Nanoseconds()
				}
				b.StopTimer()
				sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
				b.ReportMetric(float64(bundleBenchmarkPercentile(samples, 0.50)), "p50-ns")
				b.ReportMetric(float64(bundleBenchmarkPercentile(samples, 0.95)), "p95-ns")
				b.ReportMetric(float64(bundleBenchmarkPercentile(samples, 0.99)), "p99-ns")
			})
		}
	}
}

// BenchmarkBundleResolverTailFallback measures the sandboxer path resolver,
// lazy Reader opens, ordered clean misses, and the final remote selection. The
// cold cases include root open/close; warm cases reuse the per-root Reader
// cache and perform no repeated file opens.
func BenchmarkBundleResolverTailFallback(b *testing.B) {
	b.StopTimer()
	customerKey := [32]byte{0xd1, 0xd2, 0xd3}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sourceDir := b.TempDir()
	_, sourcePath, _ := writeResolverBundle(b, sourceDir, "resolver-bench-source", cfg, keyFn, nil, false, 0x31)
	target := store.ContentKey{0xff, 0xee, 0xdd}
	remote := benchmarkTailRemote{}
	for _, count := range []int{0, 1, 8, 32} {
		refs := make([]string, count)
		locations := make(config.RefLocations, count)
		for index := range refs {
			name := fmt.Sprintf("source-%02d", index)
			refs[index] = "file://" + filepath.Base(sourcePath) + "@location:" + name
			locations[name] = sourceDir
		}
		_, currentPath, _ := writeResolverBundle(b, b.TempDir(), fmt.Sprintf("resolver-current-%d", count), cfg, keyFn, refs, false, byte(0x51+count))

		b.Run(fmt.Sprintf("Cold/%dRefs", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				opened, err := artifact.OpenFileWithLocations(context.Background(), currentPath,
					manifest.Ref{Scheme: manifest.RefSchemeFile, Path: currentPath}, cfg, keyFn, remote, locations, nil, false)
				if err != nil {
					b.Fatal(err)
				}
				source, err := opened.ManifestFetcher().SelectManifest(context.Background(), target)
				if err != nil || source.Reader != nil {
					_ = opened.Close()
					b.Fatalf("tail selection = %#v, %v", source, err)
				}
				if err := opened.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})

		b.Run(fmt.Sprintf("Warm/%dRefs", count), func(b *testing.B) {
			opened, err := artifact.OpenFileWithLocations(context.Background(), currentPath,
				manifest.Ref{Scheme: manifest.RefSchemeFile, Path: currentPath}, cfg, keyFn, remote, locations, nil, false)
			if err != nil {
				b.Fatal(err)
			}
			defer opened.Close()
			if _, err := opened.ManifestFetcher().SelectManifest(context.Background(), target); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := opened.ManifestFetcher().SelectManifest(context.Background(), target); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type benchmarkTailRemote struct{}

func (benchmarkTailRemote) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	return nil, fmt.Errorf("benchmark remote should only be selected")
}

func bundleBenchmarkPercentile(sorted []int64, percentile float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(percentile*float64(len(sorted)-1))]
}

func bundleBenchmarkBytes(size int) []byte {
	result := make([]byte, size)
	x := uint64(0x1234_5678_9abc_def0)
	for i := range result {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		result[i] = byte(x)
	}
	return result
}
