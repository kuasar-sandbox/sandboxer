package uffd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/codec"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

type benchmarkFaultStrategy uint8

const benchmarkBatchBytes = 1 << 20

const (
	benchmarkSyncFullBatch benchmarkFaultStrategy = iota
	benchmarkFaultFirstOnly
	benchmarkFaultFirstTail
)

func (s benchmarkFaultStrategy) String() string {
	switch s {
	case benchmarkSyncFullBatch:
		return "A_SyncFullBatch"
	case benchmarkFaultFirstOnly:
		return "B_FaultFirstNoTail"
	case benchmarkFaultFirstTail:
		return "C_FaultFirstSerialTail"
	default:
		return "Unknown"
	}
}

type benchmarkSnapshotFixture struct {
	name   string
	source SnapshotReader
	size   uint64
}

type benchmarkAccessPattern struct {
	name     string
	random   bool
	parallel bool
}

// BenchmarkUFFDFaultStrategies is the standardized in-process A/B/C harness:
//
//	A. the former synchronous full-range source read + population;
//	B. fault-first with only the urgent page;
//	C. fault-first plus the source-aware serial-tail unit.
//
// It isolates source and population work from a real userfaultfd syscall so it
// runs in CI without KVM. End-to-end cold page-cache, NFS, vCPU wake latency,
// CPU, and RSS measurements use the same names in the external restore matrix
// documented in docs/sandbox.md. Set KUASAR_UFFD_BENCH_NFS_ARTIFACT to include
// a canonical tarstream artifact stored on an NFS mount.
func BenchmarkUFFDFaultStrategies(b *testing.B) {
	fixtures := []benchmarkSnapshotFixture{
		benchmarkOrdinaryFixture(b),
		benchmarkManifestFixture(b, true),
		benchmarkManifestFixture(b, false),
		benchmarkTarFixture(b, false),
		benchmarkTarFixture(b, true),
		{name: "Zero", source: ZeroSource{}, size: 4 * benchmarkBatchBytes},
		{name: "Released", source: ZeroSource{}, size: 4 * benchmarkBatchBytes},
	}
	if path := os.Getenv("KUASAR_UFFD_BENCH_NFS_ARTIFACT"); path != "" {
		stream, err := fetch.OpenTarStream(path)
		if err != nil {
			b.Fatalf("open NFS benchmark artifact: %v", err)
		}
		b.Cleanup(func() { _ = stream.Close() })
		source, err := NewStreamSnapshotSource(stream, stream.Size())
		if err != nil {
			b.Fatal(err)
		}
		fixtures = append(fixtures, benchmarkSnapshotFixture{name: "NFS", source: source, size: stream.Size()})
	}

	patterns := []benchmarkAccessPattern{
		{name: "Sequential1VCPU"},
		{name: "Random1VCPU", random: true},
		{name: "Sequential2VCPU", parallel: true},
		{name: "Random2VCPU", random: true, parallel: true},
	}
	strategies := []benchmarkFaultStrategy{
		benchmarkSyncFullBatch,
		benchmarkFaultFirstOnly,
		benchmarkFaultFirstTail,
	}
	for _, fixture := range fixtures {
		fixture := fixture
		for _, pattern := range patterns {
			pattern := pattern
			for _, strategy := range strategies {
				strategy := strategy
				b.Run(fixture.name+"/"+pattern.name+"/"+strategy.String(), func(b *testing.B) {
					benchmarkFaultStrategyRun(b, fixture, pattern, strategy)
				})
			}
		}
	}
}

func benchmarkFaultStrategyRun(b *testing.B, fixture benchmarkSnapshotFixture, pattern benchmarkAccessPattern, strategy benchmarkFaultStrategy) {
	b.Helper()
	if fixture.size < PageSize {
		b.Fatalf("fixture %s is smaller than one page", fixture.name)
	}
	var sourceBytes atomic.Uint64
	var uffdBytes atomic.Uint64
	var firstErr error
	var errOnce sync.Once
	recordErr := func(err error) {
		if err != nil {
			errOnce.Do(func() { firstErr = err })
		}
	}
	runOne := func(buf []byte, offset uint64) {
		sourceN, uffdN, err := benchmarkResolveFault(context.Background(), fixture.source, offset, strategy, buf)
		sourceBytes.Add(sourceN)
		uffdBytes.Add(uffdN)
		recordErr(err)
	}

	b.ReportAllocs()
	b.SetBytes(PageSize)
	if pattern.parallel {
		oldProcs := runtime.GOMAXPROCS(2)
		defer runtime.GOMAXPROCS(oldProcs)
		var workerSeed atomic.Uint64
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			buf := make([]byte, benchmarkBatchBytes)
			seed := workerSeed.Add(0x9e37_79b9)
			var iteration uint64
			for pb.Next() {
				offset := benchmarkFaultOffset(fixture.size, pattern.random, iteration, &seed)
				runOne(buf, offset)
				iteration++
			}
		})
	} else {
		buf := make([]byte, benchmarkBatchBytes)
		seed := uint64(0x1234_5678)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			offset := benchmarkFaultOffset(fixture.size, pattern.random, uint64(i), &seed)
			runOne(buf, offset)
		}
	}
	b.StopTimer()
	if firstErr != nil {
		b.Fatal(firstErr)
	}
	if b.N > 0 {
		b.ReportMetric(float64(sourceBytes.Load())/float64(b.N), "source-B/op")
		b.ReportMetric(float64(uffdBytes.Load())/float64(b.N), "uffd-B/op")
	}
}

func benchmarkFaultOffset(size uint64, random bool, iteration uint64, seed *uint64) uint64 {
	// Keep the 1 MiB query inside the fixture so the former full-batch strategy
	// remains comparable while the serial-tail strategy stays within its
	// per-run bounded fill policy.
	offsetSpan := size
	if size > benchmarkBatchBytes {
		offsetSpan = size - benchmarkBatchBytes + PageSize
	}
	pages := offsetSpan / PageSize
	if !random {
		return (iteration % pages) * PageSize
	}
	x := *seed
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	*seed = x
	return (x % pages) * PageSize
}

func benchmarkResolveFault(ctx context.Context, source SnapshotReader, offset uint64, strategy benchmarkFaultStrategy, buf []byte) (uint64, uint64, error) {
	run, err := source.RunAt(offset, benchmarkBatchBytes)
	if err != nil {
		return 0, 0, err
	}
	if run.End()-run.Offset() < PageSize {
		return 0, 0, fmt.Errorf("benchmark Run [%d,%d) does not cover a page", run.Offset(), run.End())
	}
	visible := run.End() - run.Offset()
	aligned := (visible / PageSize) * PageSize
	if run.Kind() != sparse.Data {
		switch strategy {
		case benchmarkSyncFullBatch:
			benchmarkPopulationSink.Add(aligned)
			return 0, aligned, nil
		case benchmarkFaultFirstOnly:
			benchmarkPopulationSink.Add(PageSize)
			return 0, PageSize, nil
		case benchmarkFaultFirstTail:
			tail := min(uint64(zeroNeighborTailBytes), aligned-PageSize)
			benchmarkPopulationSink.Add(PageSize + tail)
			return 0, PageSize + tail, nil
		}
	}

	read := func(dst []byte, innerOffset uint64) error {
		n, err := run.ReadAt(ctx, dst, innerOffset)
		if err != nil {
			return err
		}
		if n != len(dst) {
			return io.ErrUnexpectedEOF
		}
		benchmarkDataSink.Store(uint32(dst[0]))
		return nil
	}
	switch strategy {
	case benchmarkSyncFullBatch:
		if err := read(buf[:aligned], 0); err != nil {
			return 0, 0, err
		}
		benchmarkPopulationSink.Add(aligned)
		return aligned, aligned, nil
	case benchmarkFaultFirstOnly:
		if err := read(buf[:PageSize], 0); err != nil {
			return 0, 0, err
		}
		benchmarkPopulationSink.Add(PageSize)
		return PageSize, PageSize, nil
	case benchmarkFaultFirstTail:
		if _, ok := run.(fetch.ChunkRun); ok {
			fill := min(uint64(dataFaultFillBytes), aligned)
			if err := read(buf[:fill], 0); err != nil {
				return 0, 0, err
			}
			benchmarkPopulationSink.Add(fill)
			return fill, fill, nil
		}
		if err := read(buf[:PageSize], 0); err != nil {
			return 0, 0, err
		}
		tail := min(uint64(ordinaryDataNeighborTailBytes), aligned-PageSize)
		if tail > 0 {
			if err := read(buf[:tail], PageSize); err != nil {
				return 0, 0, err
			}
		}
		benchmarkPopulationSink.Add(PageSize + tail)
		return PageSize + tail, PageSize + tail, nil
	default:
		return 0, 0, fmt.Errorf("unknown benchmark strategy %d", strategy)
	}
}

var benchmarkPopulationSink atomic.Uint64
var benchmarkDataSink atomic.Uint32

func benchmarkOrdinaryFixture(b *testing.B) benchmarkSnapshotFixture {
	b.Helper()
	data := bytes.Repeat([]byte{0x41}, 4*benchmarkBatchBytes)
	source, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), nil)
	if err != nil {
		b.Fatal(err)
	}
	snapshot, err := NewStreamSnapshotSource(testStream{Source: source}, uint64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	return benchmarkSnapshotFixture{name: "OrdinaryData", source: snapshot, size: uint64(len(data))}
}

func benchmarkManifestFixture(b *testing.B, reuse bool) benchmarkSnapshotFixture {
	b.Helper()
	chunk := bytes.Repeat([]byte{0x52}, benchmarkBatchBytes)
	hash := sha256.Sum256(chunk)
	const chunks = 4
	entries := make([]codec.ChunkEntry, chunks)
	for i := range entries {
		entries[i] = codec.ChunkEntry{
			Offset:         uint64(i * benchmarkBatchBytes),
			Size:           benchmarkBatchBytes,
			CiphertextHash: hash,
		}
	}
	m := &codec.Manifest{Version: codec.Version1, ImageSize: chunks * benchmarkBatchBytes, Entries: entries}
	stream, getter := openSnapshotManifest(b, m, map[store.ContentKey][]byte{hash: chunk})
	getter.reuseChunk = reuse
	snapshot, err := NewStreamSnapshotSource(stream, m.ImageSize)
	if err != nil {
		b.Fatal(err)
	}
	name := "ManifestColdCopy"
	if reuse {
		name = "ManifestHit"
	}
	return benchmarkSnapshotFixture{name: name, source: snapshot, size: m.ImageSize}
}

func benchmarkTarFixture(b *testing.B, encrypted bool) benchmarkSnapshotFixture {
	b.Helper()
	data := bytes.Repeat([]byte{0x6d}, 4*benchmarkBatchBytes)
	source, err := sparse.NewSource(bytes.NewReader(data), uint64(len(data)), nil)
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(b.TempDir(), "memory.snapshot")
	out, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	var writeOptions []tarstream.WriteOption
	var readOptions []tarstream.ReadOption
	name := "LocalPlaintextTar"
	if encrypted {
		name = "LocalEncryptedTar"
		codec, err := manifestcrypto.NewTarStreamCodec([32]byte{2, 4, 6, 8})
		if err != nil {
			b.Fatal(err)
		}
		writeOptions = append(writeOptions, tarstream.WithCodec(codec, false))
		readOptions = append(readOptions, tarstream.WithCodec(codec, true))
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), out, "memory", source, writeOptions...)
	if err != nil {
		b.Fatal(err)
	}
	if err := out.Close(); err != nil {
		b.Fatal(err)
	}
	readOptions = append(readOptions, tarstream.WithExpectedDigest(scheme, digest))
	stream, err := fetch.OpenTarStream(path, readOptions...)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = stream.Close() })
	snapshot, err := NewStreamSnapshotSource(stream, uint64(len(data)))
	if err != nil {
		b.Fatal(err)
	}
	return benchmarkSnapshotFixture{name: name, source: snapshot, size: uint64(len(data))}
}
