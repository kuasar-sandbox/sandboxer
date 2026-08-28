package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
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

type closeTrackingIngester struct {
	closes int
	err    error
}

type invalidRunSource struct {
	size uint64
	run  sparse.Run
}

func (s *invalidRunSource) Size() uint64 { return s.size }
func (s *invalidRunSource) RunAt(uint64, uint64) (sparse.Run, error) {
	return s.run, nil
}
func (*invalidRunSource) ReadAt(context.Context, []byte, uint64) (int, error) { return 0, nil }
func (*invalidRunSource) Close() error                                        { return nil }

func TestConsumeSourceRejectsInvalidRun(t *testing.T) {
	if err := consumeSource(context.Background(), &invalidRunSource{size: 8}); err == nil || !strings.Contains(err.Error(), "invalid sparse run") {
		t.Fatalf("consumeSource nil-run error = %v", err)
	}
	dense := sparse.Dense(bytes.NewReader(make([]byte, 8)), 8)
	wrongOffset, err := dense.RunAt(1, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumeSource(context.Background(), &invalidRunSource{size: 8, run: wrongOffset}); err == nil || !strings.Contains(err.Error(), "invalid sparse run") {
		t.Fatalf("consumeSource wrong-offset error = %v", err)
	}
}

func (*closeTrackingIngester) Ingest(context.Context, sparse.Source, ingest.IngestOption) (*ingest.Result, error) {
	return &ingest.Result{}, nil
}

func (i *closeTrackingIngester) Close() error {
	i.closes++
	return i.err
}

func TestIngestSinkCloseIsIdempotent(t *testing.T) {
	wantErr := errors.New("injected ingester close failure")
	ingester := &closeTrackingIngester{err: wantErr}
	sink := NewIngestSink(ingester, nil)
	if err := sink.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("first Close error = %v, want %v", err, wantErr)
	}
	if err := sink.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("second Close error = %v, want %v", err, wantErr)
	}
	if ingester.closes != 1 {
		t.Fatalf("ingester closes = %d, want 1", ingester.closes)
	}
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

func TestFileSinkRejectsUnsafeAliasIDBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	sink := NewFileSink(dir, "../escape", nil, false, nil)
	if _, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(make([]byte, 4096)), nil); err == nil || !strings.Contains(err.Error(), "safe alias component") {
		t.Fatalf("AbsorbOverlay error = %v, want unsafe alias rejection", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsafe alias produced output entries: %v", entries)
	}
}

func TestFileSinkRefusesToReplaceNonSymlinkAlias(t *testing.T) {
	dir := t.TempDir()
	sink := NewFileSink(dir, "sid", nil, false, nil)
	ref, path, err := sink.AbsorbSnapshot(context.Background(), testSnapshotSource(t,
		make([]byte, 4096), nil,
		[]byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")))
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "sid.snapshot")
	const sentinel = "user-owned"
	if err := os.WriteFile(alias, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(context.Background(), ref, path); err == nil || !strings.Contains(err.Error(), "refuses to replace a non-symlink") {
		t.Fatalf("CommitSnapshot error = %v, want non-symlink rejection", err)
	}
	body, err := os.ReadFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != sentinel {
		t.Fatalf("non-symlink alias content = %q, want %q", body, sentinel)
	}
}
