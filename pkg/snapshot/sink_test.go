package snapshot

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func testSnapshotSource(t testing.TB, memory []byte, holes []sparse.Extent, snapshotConfig []byte) sparse.Source {
	t.Helper()
	logical, err := snapshotfile.BuildSource(&seekerSource{
		rs: bytes.NewReader(memory), size: uint64(len(memory)), holes: holes,
	}, []byte("{}"), []byte("{}"), snapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	return logical
}

// FileSink must pack content-addressed tarstream artifacts: the envelope
// carries the hole map (no OS sparseness needed), the name comes from the
// embedded digest marker, and Snapshot S keeps its [memory][ZIP] layout.
func TestFileSinkArtifacts(t *testing.T) {
	const size = 1 << 20
	mem := make([]byte, size)
	copy(mem[0:4], "HEAD")
	copy(mem[size-4:], "TAIL")
	holes := []sparse.Extent{{Offset: 4, Size: size - 8}}
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	zipTail, err := snapshotfile.BuildZIP([]byte("{}"), []byte("{}"), snapshotConfig)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	sink := NewFileSink(dir, "sid1", nil, false, nil)
	ctx := context.Background()

	ref, path, err := sink.AbsorbOverlay(ctx, bytes.NewReader(mem), holes)
	if err != nil {
		t.Fatal(err)
	}
	// Content address comes from the digest marker written with the payload.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src, _, err := tarstream.SourceAt(bytes.NewReader(raw), int64(len(raw)), "")
	if err != nil {
		t.Fatal(err)
	}
	digester, ok := src.(tarstream.Digester)
	if !ok {
		t.Fatal("overlay source has no digest marker")
	}
	scheme, digest := digester.Digest()
	wantRef := "file://" + digest + ".overlay@" + scheme + ":" + digest
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

	// Snapshot S: [memory][ZIP] as one entry named "snapshot", with the alias
	// committed only after the logical root is complete.
	bref, bpath, err := sink.AbsorbSnapshot(ctx, testSnapshotSource(t, mem, holes, snapshotConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(ctx, bref, bpath); err != nil {
		t.Fatal(err)
	}
	braw, err := os.ReadFile(bpath)
	if err != nil {
		t.Fatal(err)
	}
	bsrc, _, err := tarstream.SourceAt(bytes.NewReader(braw), int64(len(braw)), "")
	if err != nil {
		t.Fatal(err)
	}
	bdigester, ok := bsrc.(tarstream.Digester)
	if !ok {
		t.Fatal("snapshot source has no digest marker")
	}
	bscheme, bdigest := bdigester.Digest()
	if want := "file://" + bdigest + ".snapshot@" + bscheme + ":" + bdigest; bref != want {
		t.Fatalf("snapshot ref = %s, want %s", bref, want)
	}
	bv, err := tarstream.ReadSeekFrom(bytes.NewReader(braw), "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if bv.Size() != size+int64(len(zipTail)) {
		t.Fatalf("Snapshot S logical size = %d", bv.Size())
	}
	ball, _ := io.ReadAll(bv)
	if !bytes.Equal(ball[:size], mem) || !bytes.Equal(ball[size:], zipTail) {
		t.Fatal("Snapshot S [memory][ZIP] layout mismatch")
	}
	link, err := os.Readlink(filepath.Join(dir, "sid1.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Base(bpath) {
		t.Fatalf("symlink → %s, want %s", link, filepath.Base(bpath))
	}
}
