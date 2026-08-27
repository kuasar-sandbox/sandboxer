package snapshotfile_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	. "github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestBuildZIPDeterministic(t *testing.T) {
	configJSON := []byte(`{"a":1}`)
	stateJSON := []byte(`{"state":"ok"}`)
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n")
	a, err := BuildZIP(configJSON, stateJSON, snapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildZIP(configJSON, stateJSON, snapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Snapshot ZIP is not deterministic")
	}
	reader, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ConfigJSONName, StateJSONName, SnapshotCfgName}
	if len(reader.File) != len(want) {
		t.Fatalf("ZIP entries = %d, want %d", len(reader.File), len(want))
	}
	for i, file := range reader.File {
		if file.Name != want[i] || file.Method != zip.Store {
			t.Fatalf("ZIP entry[%d] = %s method=%d", i, file.Name, file.Method)
		}
	}
}

type testStream struct {
	sparse.Source
	closes int
}

type nilRunStream struct{ *testStream }

func (*nilRunStream) RunAt(uint64, uint64) (sparse.Run, error) { return nil, nil }

func TestBuildSourceRejectsInvalidMemoryRun(t *testing.T) {
	memory := &nilRunStream{testStream: &testStream{Source: sparse.Dense(bytes.NewReader(make([]byte, 8)), 8)}}
	logical, err := BuildSource(memory, []byte("{}"), []byte("{}"), []byte("version: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logical.RunAt(0, 8); err == nil || !strings.Contains(err.Error(), "invalid run") {
		t.Fatalf("Snapshot appended RunAt error = %v, want invalid run", err)
	}
}

func (s *testStream) Close() error {
	s.closes++
	return nil
}

func snapshotBytes(t *testing.T) ([]byte, int) {
	t.Helper()
	memory := bytes.Repeat([]byte{0x5a}, 8192)
	tail, err := BuildZIP(
		[]byte(`{"memory":{"size":8192}}`),
		[]byte(`{"state":"ok"}`),
		[]byte("version: 1\nsandbox_ref: manifest://0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return append(memory, tail...), len(memory)
}

func openTestStream(t *testing.T, body []byte, memorySize int) *testStream {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), []sparse.Extent{{Offset: 4096, Size: 4096}})
	if err != nil {
		t.Fatal(err)
	}
	return &testStream{Source: source}
}

func TestOpenStrictSnapshotAndMemorySection(t *testing.T) {
	body, memorySize := snapshotBytes(t)
	stream := openTestStream(t, body, memorySize)
	root, err := Open(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	if root.ArchiveBase != uint64(memorySize) || root.Memory.Size() != uint64(memorySize) {
		t.Fatalf("archive base/memory size = %d/%d", root.ArchiveBase, root.Memory.Size())
	}
	run, err := root.Memory.RunAt(4096, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind() != sparse.Hole {
		t.Fatalf("memory hole became %v", run.Kind())
	}
	if _, err := root.Memory.RunAt(uint64(memorySize), 1); !errors.Is(err, io.EOF) {
		t.Fatalf("memory exposed ZIP tail: %v", err)
	}
	if err := root.Memory.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if stream.closes != 1 {
		t.Fatalf("carrier closed %d times", stream.closes)
	}
}

func TestMemorySectionRejectsNilCarrierRun(t *testing.T) {
	body, memorySize := snapshotBytes(t)
	stream := &nilRunStream{testStream: openTestStream(t, body, memorySize)}
	root, err := Open(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := root.Memory.RunAt(0, 4096); err == nil || !strings.Contains(err.Error(), "invalid run") {
		t.Fatalf("Memory.RunAt error = %v", err)
	}
}

func TestOpenRejectsMalformedSnapshotZIP(t *testing.T) {
	body, memorySize := snapshotBytes(t)
	const eocdSize = 22
	const centralHeaderSize = 46
	const localHeaderSize = 30
	eocd := len(body) - eocdSize
	centralSize := int(binary.LittleEndian.Uint32(body[eocd+12 : eocd+16]))
	centralOffset := int(binary.LittleEndian.Uint32(body[eocd+16 : eocd+20]))
	base := eocd - centralSize - centralOffset
	central := base + centralOffset

	tests := map[string]func([]byte) []byte{
		"trailing bytes": func(in []byte) []byte { return append(in, 0) },
		"comment": func(in []byte) []byte {
			binary.LittleEndian.PutUint16(in[len(in)-2:], 1)
			return in
		},
		"multi disk": func(in []byte) []byte {
			binary.LittleEndian.PutUint16(in[eocd+4:eocd+6], 1)
			return in
		},
		"ZIP64": func(in []byte) []byte {
			binary.LittleEndian.PutUint32(in[eocd+16:eocd+20], ^uint32(0))
			return in
		},
		"unknown entry": func(in []byte) []byte {
			copy(in[central+centralHeaderSize:central+centralHeaderSize+len(ConfigJSONName)], []byte("unknown.jsn"))
			return in
		},
		"local central mismatch": func(in []byte) []byte {
			in[base+localHeaderSize] ^= 1
			return in
		},
		"CRC": func(in []byte) []byte {
			in[base+localHeaderSize+len(ConfigJSONName)] ^= 1
			return in
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			corrupt := mutate(append([]byte(nil), body...))
			stream := openTestStream(t, corrupt, memorySize)
			if _, err := Open(context.Background(), stream); err == nil {
				t.Fatal("malformed Snapshot accepted")
			}
			if stream.closes != 1 {
				t.Fatalf("failed carrier closed %d times", stream.closes)
			}
		})
	}
}
