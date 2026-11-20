package artifact

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

type countedMemoryMetadata struct {
	sparse.Source
	resolves *int
}

func (s countedMemoryMetadata) RunAt(off, limit uint64) (sparse.Run, error) {
	*s.resolves++
	return s.Source.RunAt(off, limit)
}

type countedMemoryReader struct {
	*bytes.Reader
	reads int
}

func (r *countedMemoryReader) ReadAt(b []byte, off int64) (int, error) {
	r.reads++
	return r.Reader.ReadAt(b, off)
}

func TestMemoryCheckpointLayeringKeepsCandidateTraversalBounded(t *testing.T) {
	for _, empty := range []bool{true, false} {
		t.Run(fmt.Sprintf("empty-top=%t", empty), func(t *testing.T) {
			ctx := context.Background()
			const size = 4 * 4096
			reader := &countedMemoryReader{Reader: bytes.NewReader(checkpointPageBytes("abxx"))}
			base, _, err := snapshot.CaptureMemory(reader, &memory.CaptureView{
				Size: size, Materialized: []byte{3}, Dirty: []byte{3}, Released: []byte{4},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			resolves := 0
			leaf := countedMemoryMetadata{Source: base, resolves: &resolves}
			materialized := byte(0)
			if !empty {
				materialized = 8
			}
			top, _, err := snapshot.CaptureMemory(reader, &memory.CaptureView{
				Size: size, Materialized: []byte{materialized}, Dirty: []byte{materialized}, Released: []byte{0},
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &snapshot.Config{Version: 1}
			node := &memoryOpenNode{config: cfg, local: true, values: leaf, candidates: leaf, dirty: leaf}
			for i := 0; i < 32; i++ {
				frame := &memoryOpenFrame{descriptor: &memorycheckpoint.Descriptor{Size: size}, config: cfg,
					top: top, candidates: top, topEmpty: empty, parents: []*memoryOpenNode{node}}
				node, _, err = frame.compose(ctx)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := node.values.RunAt(17, size-17); err != nil {
				t.Fatal(err)
			}
			if reader.reads != 0 || resolves > 1000 {
				t.Fatalf("metadata traversal multiplied or read payload: payload=%d resolves=%d", reader.reads, resolves)
			}
			if empty {
				assertMemoryPages(t, node.values, "DDZH", "ab\x00\x00")
				if reader.reads != 2 {
					t.Fatalf("payload reads=%d want=2", reader.reads)
				}
			} else {
				assertMemoryPages(t, node.values, "DDZD", "ab\x00x")
				if reader.reads != 3 {
					t.Fatalf("payload reads=%d want=3", reader.reads)
				}
			}
		})
	}
}

func TestMemoryCheckpointLongFileHistoryHasNoCheckpointCountLimit(t *testing.T) {
	fixture := newMemoryCheckpointFixture(t)
	for i := 0; i < 1030; i++ {
		fixture.ref, fixture.path, fixture.descriptor = writeLocalMemory(t, fixture.directory,
			fmt.Sprintf("chain-%d", i), fixture.eRef, []string{fixture.ref}, []string{fixture.external},
			"xxxxxx", 0, 0, 0, false)
	}
	root := openLocalFixture(t, fixture)
	// Every old clean W copy disappeared on the first C. Old dirty values and
	// explicit zeros remain; absent pages still inherit the external suffix.
	assertMemoryPages(t, root.Memory, "DDZHZH", "bc\x00\x00\x00\x00")
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.CleanupCheckpoint(context.Background(), Checkpoint{
		Directory: fixture.directory, SandboxID: "sid", SnapshotRef: fixture.ref, SandboxRef: fixture.eRef,
	}); err != nil {
		t.Fatal(err)
	}
	root = openLocalFixture(t, fixture)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}
