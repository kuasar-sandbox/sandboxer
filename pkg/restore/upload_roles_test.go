package restore

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

type workingSetPublishFixture struct {
	dir         string
	rootPath    string
	memoryPath  string
	manifestRef string
	locatedRef  string
}

func newWorkingSetPublishFixture(t *testing.T) workingSetPublishFixture {
	t.Helper()
	dir := t.TempDir()

	// B remains a valid memory artifact, but its historical disk tops have
	// deliberately been removed from the minimal W artifact set. A recursive
	// root-graph interpretation of B would fail on these refs.
	base := &SnapshotCfg{}
	base.Resources.Capacity.Memory = "4KiB"
	base.Boot.Root.Overlay = &SnapOverlayCfg{Base: "file://deleted-b-root.overlay"}
	base.Boot.Disks = []SnapDiskNode{
		{Base: "file://deleted-b-data-0.overlay"},
		{Overlay: &SnapOverlayCfg{Base: "file://deleted-b-data-1.overlay"}},
	}
	memoryPath := writePublishSnapshot(t, dir, base)

	diskRefs := make([]string, 3)
	for i, value := range []byte{0x31, 0x52, 0x73} {
		path, _ := writePublishArtifact(t, dir, ".overlay", bytes.Repeat([]byte{value}, 4096))
		diskRefs[i] = "file://" + filepath.Base(path)
	}
	manifestRef := "manifest://" + strings.Repeat("a", 64)
	locatedRef := "file://" + strings.Repeat("b", 64) + ".snapshot@location:existing"
	root := &SnapshotCfg{FromRefs: []string{
		"file://" + filepath.Base(memoryPath),
		manifestRef,
		locatedRef,
		"file://" + filepath.Base(memoryPath),
	}}
	root.Resources.Capacity.Memory = "4KiB"
	root.Boot.Root.Overlay = &SnapOverlayCfg{Base: diskRefs[0]}
	root.Boot.Disks = []SnapDiskNode{
		{Base: diskRefs[1]},
		{Overlay: &SnapOverlayCfg{Base: diskRefs[2]}},
	}
	return workingSetPublishFixture{
		dir: dir, rootPath: writePublishSnapshot(t, dir, root), memoryPath: memoryPath,
		manifestRef: manifestRef, locatedRef: locatedRef,
	}
}

func TestManifestPublisherTreatsFromRefsAsOpaqueMemoryLayers(t *testing.T) {
	fixture := newWorkingSetPublishFixture(t)
	recorder := &recordingIngester{bodies: map[string][]byte{}}
	p := newSnapshotPublisher(context.Background(), nil, false, nil)
	p.ing = recorder

	rootRef, err := p.publishRootSnapshot(fixture.rootPath, manifest.Ref{})
	if err != nil {
		t.Fatal(err)
	}
	// B is published once despite appearing twice, the three W disk tops are
	// leaves, and W itself is rebuilt once. Deleted B disk tops are never read.
	if recorder.calls != 5 {
		t.Fatalf("manifest ingest calls = %d, want 5 (B + 3 W disks + W root)", recorder.calls)
	}
	publishedRoot := parseRecordedSnapshot(t, recorder, rootRef)
	assertPublishedWorkingSetRefs(t, publishedRoot, fixture.manifestRef, fixture.locatedRef)

	memoryRef := publishedRoot.FromRefs[0]
	if memoryRef == rootRef {
		t.Fatal("W self and B memory layer collapsed to the same ref")
	}
	publishedBase := parseRecordedSnapshot(t, recorder, memoryRef)
	if got := publishedBase.Boot.Root.Overlay.Base; got != "file://deleted-b-root.overlay" {
		t.Fatalf("opaque B snapshot.cfg was rewritten: root top = %q", got)
	}
	if got := publishedBase.Boot.Disks[0].Base; got != "file://deleted-b-data-0.overlay" {
		t.Fatalf("opaque B data graph was rewritten: data top = %q", got)
	}

	realMemory, err := filepath.EvalSymlinks(fixture.memoryPath)
	if err != nil {
		t.Fatal(err)
	}
	realMemory, err = filepath.Abs(realMemory)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[publishRole]bool{}
	for key := range p.done {
		if key.RealPath == realMemory {
			roles[key.Role] = true
		}
	}
	if len(roles) != 1 || !roles[publishMemoryLayer] {
		t.Fatalf("B cache roles = %v, want memory-layer only", roles)
	}
}

func TestLocationPublisherTreatsFromRefsAsOpaqueMemoryLayers(t *testing.T) {
	fixture := newWorkingSetPublishFixture(t)
	targetDir := t.TempDir()
	rootRef, err := PublishLocalToLocation(context.Background(), fixture.rootPath, "published", targetDir, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	publishedRoot := readLocatedSnapshot(t, targetDir, rootRef)
	assertPublishedWorkingSetRefs(t, publishedRoot, fixture.manifestRef, fixture.locatedRef)
	if publishedRoot.FromRefs[0] == rootRef {
		t.Fatal("located W self and B memory layer collapsed to the same ref")
	}
	publishedBase := readLocatedSnapshot(t, targetDir, publishedRoot.FromRefs[0])
	if got := publishedBase.Boot.Root.Overlay.Base; got != "file://deleted-b-root.overlay" {
		t.Fatalf("located opaque B snapshot.cfg was rewritten: root top = %q", got)
	}
	if got := publishedBase.Boot.Disks[1].Overlay.Base; got != "file://deleted-b-data-1.overlay" {
		t.Fatalf("located opaque B data graph was rewritten: data top = %q", got)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("located artifact count = %d, want 5 (B + 3 W disks + W root): %v", len(entries), entries)
	}
}

func TestPublisherMemoizationIncludesRole(t *testing.T) {
	dir := t.TempDir()
	leafPath, _ := writePublishArtifact(t, dir, ".overlay", bytes.Repeat([]byte{0x44}, 4096))
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: "file://" + filepath.Base(leafPath)}
	sharedPath := writePublishSnapshot(t, dir, cfg)
	recorder := &recordingIngester{bodies: map[string][]byte{}}
	p := newSnapshotPublisher(context.Background(), nil, false, nil)
	p.ing = recorder

	rootRef, err := p.publishRootSnapshot(sharedPath, manifest.Ref{})
	if err != nil {
		t.Fatal(err)
	}
	memoryRef, err := p.publishMemorySnapshotLayer("memory parent", sharedPath, manifest.Ref{})
	if err != nil {
		t.Fatal(err)
	}
	if rootRef == memoryRef {
		t.Fatal("root rewrite reused the opaque memory-layer result")
	}
	rewritten := parseRecordedSnapshot(t, recorder, rootRef)
	opaque := parseRecordedSnapshot(t, recorder, memoryRef)
	if ref, err := manifest.ParseRef(rewritten.Boot.Root.Overlay.Base); err != nil || !ref.Portable() {
		t.Fatalf("root role did not rewrite its leaf: ref=%q err=%v", rewritten.Boot.Root.Overlay.Base, err)
	}
	if opaque.Boot.Root.Overlay.Base != "file://"+filepath.Base(leafPath) {
		t.Fatalf("memory role rewrote opaque snapshot.cfg: %q", opaque.Boot.Root.Overlay.Base)
	}
	if recorder.calls != 3 {
		t.Fatalf("ingest calls = %d, want leaf + rewritten root + opaque memory", recorder.calls)
	}
	realPath, err := filepath.Abs(sharedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.done[publishCacheKey{RealPath: realPath, Role: publishRootGraph}]; !ok {
		t.Fatal("root-graph cache entry missing")
	}
	if _, ok := p.done[publishCacheKey{RealPath: realPath, Role: publishMemoryLayer}]; !ok {
		t.Fatal("memory-layer cache entry missing")
	}
}

func TestRootPublisherCycleGuardRemainsRoleScoped(t *testing.T) {
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	path := writePublishSnapshot(t, t.TempDir(), cfg)
	realPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	p := newSnapshotPublisher(context.Background(), nil, false, nil)
	p.visiting[publishCacheKey{RealPath: realPath, Role: publishRootGraph}] = true
	if _, err := p.publishRootSnapshot(path, manifest.Ref{}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("root cycle guard error = %v", err)
	}
}

func assertPublishedWorkingSetRefs(t *testing.T, cfg *SnapshotCfg, manifestRef, locatedRef string) {
	t.Helper()
	if len(cfg.FromRefs) != 4 {
		t.Fatalf("published from_refs = %v, want four refs", cfg.FromRefs)
	}
	if cfg.FromRefs[0] != cfg.FromRefs[3] {
		t.Fatalf("duplicate local memory ref was not memoized: %v", cfg.FromRefs)
	}
	if cfg.FromRefs[1] != manifestRef || cfg.FromRefs[2] != locatedRef {
		t.Fatalf("mixed portable refs changed or reordered: %v", cfg.FromRefs)
	}
	if ref, err := manifest.ParseRef(cfg.FromRefs[0]); err != nil || !ref.Portable() {
		t.Fatalf("local memory ref was not upgraded: ref=%q err=%v", cfg.FromRefs[0], err)
	}
	for index, raw := range []string{
		cfg.Boot.Root.Overlay.Base,
		cfg.Boot.Disks[0].Base,
		cfg.Boot.Disks[1].Overlay.Base,
	} {
		if ref, err := manifest.ParseRef(raw); err != nil || !ref.Portable() {
			t.Fatalf("W disk %d was not published as a leaf: ref=%q err=%v", index, raw, err)
		}
	}
}

type recordingIngester struct {
	calls  uint64
	bodies map[string][]byte
}

func (r *recordingIngester) Ingest(ctx context.Context, source sparse.Source, _ ingest.IngestOption) (*ingest.Result, error) {
	r.calls++
	key := store.ContentKey{}
	binary.BigEndian.PutUint64(key[len(key)-8:], r.calls)
	body, err := readSparseSource(ctx, source)
	if err != nil {
		return nil, err
	}
	hexKey := manifest.HexKey(key)
	r.bodies[hexKey] = body
	return &ingest.Result{ManifestKey: key}, nil
}

func readSparseSource(ctx context.Context, source sparse.Source) ([]byte, error) {
	if source.Size() > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("source too large")
	}
	body := make([]byte, int(source.Size()))
	for offset := 0; offset < len(body); {
		end := offset + 64*1024
		if end > len(body) {
			end = len(body)
		}
		n, err := source.ReadAt(ctx, body[offset:end], uint64(offset))
		offset += n
		if err != nil && err != io.EOF {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return body, nil
}

func parseRecordedSnapshot(t *testing.T, recorder *recordingIngester, rawRef string) *SnapshotCfg {
	t.Helper()
	ref, err := manifest.ParseRef(rawRef)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := recorder.bodies[ref.Path]
	if !ok {
		t.Fatalf("no recorded body for %s", rawRef)
	}
	return parseSnapshotBytes(t, body)
}

func readLocatedSnapshot(t *testing.T, targetDir, rawRef string) *SnapshotCfg {
	t.Helper()
	ref, err := manifest.ParseRef(rawRef)
	if err != nil {
		t.Fatal(err)
	}
	identity := ref
	identity.Location = ""
	identity.Path = filepath.Join(targetDir, ref.Path)
	stream, _, _, err := openTarArtifact(identity.Path, identity, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	body := make([]byte, stream.Size())
	if n, err := stream.ReadAt(context.Background(), body, 0); n != len(body) || (err != nil && err != io.EOF) {
		t.Fatalf("read located snapshot: n=%d err=%v", n, err)
	}
	return parseSnapshotBytes(t, body)
}

func parseSnapshotBytes(t *testing.T, body []byte) *SnapshotCfg {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range zr.File {
		if file.Name != "snapshot.cfg" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		cfgBody, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ParseSnapshotCfg(cfgBody)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	t.Fatal("snapshot.cfg not found")
	return nil
}
