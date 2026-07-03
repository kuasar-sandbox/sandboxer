package snapshot

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
)

// FileSink must pack content-addressed tarstream artifacts: the envelope
// carries the hole map (no OS sparseness needed), the name is the SHA256 of
// the artifact bytes, and the bundle keeps its [memory][ZIP] entry layout.
func TestFileSinkArtifacts(t *testing.T) {
	const size = 1 << 20
	mem := make([]byte, size)
	copy(mem[0:4], "HEAD")
	copy(mem[size-4:], "TAIL")
	holes := []sparse.Extent{{Offset: 4, Size: size - 8}}
	zipTail := []byte("ZIPTRAILER")

	dir := t.TempDir()
	sink := NewFileSink(dir, "sid1", nil)
	ctx := context.Background()

	ref, path, err := sink.AbsorbOverlay(ctx, bytes.NewReader(mem), holes)
	if err != nil {
		t.Fatal(err)
	}
	// Content address = sha256 of the artifact bytes.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	wantRef := "file://" + hex.EncodeToString(sum[:]) + ".overlay"
	if ref != wantRef {
		t.Fatalf("overlay ref = %s, want %s", ref, wantRef)
	}
	// The envelope round-trips content + hole map.
	v, err := tarstream.ReadSeekFrom(bytes.NewReader(raw), "overlay")
	if err != nil {
		t.Fatal(err)
	}
	if v.Size() != size {
		t.Fatalf("overlay logical size = %d", v.Size())
	}
	gotHoles := v.Holes()
	if len(gotHoles) != 1 || gotHoles[0] != holes[0] {
		t.Fatalf("overlay holes = %v, want %v", gotHoles, holes)
	}
	got, _ := io.ReadAll(v)
	if !bytes.Equal(got, mem) {
		t.Fatal("overlay content mismatch")
	}

	// Bundle: [memory][ZIP] as one entry named "snapshot", + <sid>.snapshot symlink.
	bref, bpath, err := sink.AbsorbBundle(ctx, bytes.NewReader(mem), holes, bytes.NewReader(zipTail))
	if err != nil {
		t.Fatal(err)
	}
	braw, err := os.ReadFile(bpath)
	if err != nil {
		t.Fatal(err)
	}
	bsum := sha256.Sum256(braw)
	if want := "file://" + hex.EncodeToString(bsum[:]) + ".snapshot"; bref != want {
		t.Fatalf("bundle ref = %s, want %s", bref, want)
	}
	bv, err := tarstream.ReadSeekFrom(bytes.NewReader(braw), "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if bv.Size() != size+int64(len(zipTail)) {
		t.Fatalf("bundle logical size = %d", bv.Size())
	}
	ball, _ := io.ReadAll(bv)
	if !bytes.Equal(ball[:size], mem) || !bytes.Equal(ball[size:], zipTail) {
		t.Fatal("bundle [memory][ZIP] layout mismatch")
	}
	link, err := os.Readlink(filepath.Join(dir, "sid1.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Base(bpath) {
		t.Fatalf("symlink → %s, want %s", link, filepath.Base(bpath))
	}
}

// concatReadSeeker must present [mem][tail] as one seekable stream so
// ingest.Ingest can Seek to each data segment across the memory/ZIP boundary.
func TestConcatReadSeeker(t *testing.T) {
	mem := []byte("0123456789") // memSize = 10
	tail := []byte("ABCDEF")    // 6
	full := append(append([]byte{}, mem...), tail...)
	c := &concatReadSeeker{mem: bytes.NewReader(mem), memSize: int64(len(mem)), tail: tail}

	// (1) full sequential read reconstructs mem||tail (mem EOF must not stop it).
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("sequential = %q, want %q", got, full)
	}

	// (2) seek into the tail region, read to end.
	if _, err := c.Seek(12, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, full[12:]) {
		t.Fatalf("tail read = %q, want %q", got, full[12:])
	}

	// (3) seek to mem, ReadFull a span straddling the boundary [8,13).
	if _, err := c.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	span := make([]byte, 5)
	if _, err := io.ReadFull(c, span); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(span, full[8:13]) {
		t.Fatalf("straddle = %q, want %q", span, full[8:13])
	}

	// (4) SeekEnd reports total length.
	if end, err := c.Seek(0, io.SeekEnd); err != nil || end != int64(len(full)) {
		t.Fatalf("SeekEnd = %d, %v; want %d, nil", end, err, len(full))
	}
}

// BuildZIP must be byte-deterministic (sorted names, fixed mtime) and readable.
func TestBuildZIPDeterministic(t *testing.T) {
	entries := map[string][]byte{
		"snapshot.cfg": []byte("cfg"),
		"config.json":  []byte(`{"a":1}`),
		"state.json":   []byte("state"),
	}
	a, err := BuildZIP(entries)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildZIP(entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("BuildZIP not deterministic across calls")
	}

	zr, err := zip.NewReader(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = string(data)
	}
	for name, want := range map[string]string{"snapshot.cfg": "cfg", "config.json": `{"a":1}`, "state.json": "state"} {
		if got[name] != want {
			t.Errorf("entry %s = %q, want %q", name, got[name], want)
		}
	}
}
