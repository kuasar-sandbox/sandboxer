package artifact

import (
	"bytes"
	"context"
	"errors"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func rewriteSparse(t *testing.T, body []byte, holes ...sparse.Extent) sparse.Source {
	t.Helper()
	s, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), holes)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func rewriteSaveE(t *testing.T, sink *snapshot.FileSink, cfg *config.PortableSandboxConfig, payload sparse.Source) string {
	t.Helper()
	data, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sandboxfile.BuildSource(payload, nil, data)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := sink.AbsorbSandbox(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func rewriteSaveS(t *testing.T, sink *snapshot.FileSink, cfg *snapshot.Config, payload sparse.Source) string {
	t.Helper()
	data, err := snapshot.MarshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := snapshotfile.BuildSource(payload, []byte(`{"state":"top"}`), []byte(`{"ticks":42}`), data)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := sink.AbsorbSnapshot(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func rewriteOpen(t *testing.T, p *Publisher, raw string) *OpenedFile {
	t.Helper()
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		t.Fatal(err)
	}
	path, err := p.locations.ResolveFile(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := p.storage.OpenFileWithLocations(context.Background(), path, ref, p.locations)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func rewriteTarget(t *testing.T, storage *ProcessStorage, dir string) *Publisher {
	t.Helper()
	p, err := NewLocationPublisher(storage, "result", dir, config.RefLocations{"result": dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestRewriteReduceAnyMaterializesMemoryAndAllDisks(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	const size = 3 * 4096
	lower := bytes.Repeat([]byte{0x31}, size)
	lowerRef, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(lower), nil)
	if err != nil {
		t.Fatal(err)
	}
	top := bytes.Repeat([]byte{0x42}, size)
	holes := []sparse.Extent{{Offset: 0, Size: 4096}, {Offset: 8192, Size: 4096}}
	diskLower := bytes.Repeat([]byte{0x57}, size)
	dLower, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(diskLower), nil)
	if err != nil {
		t.Fatal(err)
	}
	diskTop := bytes.Repeat([]byte{0x68}, size)
	dTop, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(diskTop), holes)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := publishPortable(t, lowerRef)
	cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: dTop, BaseFromRefs: []string{dLower}}}}
	cfg.Mounts = []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	eRef := rewriteSaveE(t, sink, cfg, rewriteSparse(t, top, holes...))
	parentMemory := bytes.Repeat([]byte{0x71}, size)
	pRef := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: "file://unavailable.sandbox@digest:" + publishTestSHA}, rewriteSparse(t, parentMemory))
	currentMemory := bytes.Repeat([]byte{0x82}, size)
	sRef := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: []string{pRef}}, rewriteSparse(t, currentMemory, sparse.Extent{Offset: 4096, Size: 8192}))
	parsed, _ := manifest.ParseRef(sRef)
	sourcePath := filepath.Join(input, parsed.Path)
	result, err := publisher.PublishWithOptions(ctx, sourcePath, RewriteOptions{ReduceAny: true})
	if err != nil {
		t.Fatal(err)
	}
	// All source increment files may now leave the retained graph.
	if err = os.RemoveAll(input); err != nil {
		t.Fatal(err)
	}
	root, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	state, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.FromRefs) != 0 {
		t.Fatalf("memory parents remain: %v", state.FromRefs)
	}
	expectedMemory := append([]byte(nil), parentMemory...)
	copy(expectedMemory[:4096], currentMemory[:4096])
	body, err := readPublishSource(ctx, root.Memory)
	if err != nil || !bytes.Equal(body, expectedMemory) {
		t.Fatalf("memory content changed: %v", err)
	}
	if string(root.ConfigJSON) != `{"state":"top"}` || string(root.StateJSON) != `{"ticks":42}` {
		t.Fatal("execution state changed")
	}
	e, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, state.SandboxRef))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Portable.Boot.Root.Base != "self" || len(e.Portable.Boot.Root.BaseFromRefs) != 0 || len(e.Portable.Boot.Disks[0].BaseFromRefs) != 0 {
		t.Fatal("disk chains not reduced")
	}
	expectedDisk := append([]byte(nil), lower...)
	copy(expectedDisk[4096:8192], top[4096:8192])
	body, err = readPublishSource(ctx, e.Payload)
	if err != nil || !bytes.Equal(body, expectedDisk) {
		t.Fatalf("root content changed: %v", err)
	}
	d := rewriteOpen(t, publisher, e.Portable.Boot.Disks[0].Base)
	defer d.Close()
	expectedData := append([]byte(nil), diskLower...)
	copy(expectedData[4096:8192], diskTop[4096:8192])
	body, err = readPublishSource(ctx, d)
	if err != nil || !bytes.Equal(body, expectedData) {
		t.Fatalf("data disk changed: %v", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 3 {
		t.Fatalf("want only data/E/S final files: %v %v", entries, err)
	}
}

type rewriteKindSource struct {
	kind sparse.RunKind
	size uint64
	err  error
}

func (s rewriteKindSource) Size() uint64 { return s.size }
func (s rewriteKindSource) RunAt(off, limit uint64) (sparse.Run, error) {
	return rewriteKindRun{s: s, off: off, end: min(s.size, off+limit)}, nil
}
func (s rewriteKindSource) ReadAt(_ context.Context, p []byte, _ uint64) (int, error) {
	clear(p)
	if s.err != nil {
		return 0, s.err
	}
	return len(p), nil
}

type rewriteKindRun struct {
	s        rewriteKindSource
	off, end uint64
}

func (r rewriteKindRun) Offset() uint64       { return r.off }
func (r rewriteKindRun) End() uint64          { return r.end }
func (r rewriteKindRun) Kind() sparse.RunKind { return r.s.kind }
func (r rewriteKindRun) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	return r.s.ReadAt(ctx, p, off)
}
func TestRewriteSparseEquivalenceUsesHolePresentSemantics(t *testing.T) {
	zero := rewriteKindSource{kind: sparse.Zero, size: 4096}
	data := rewriteKindSource{kind: sparse.Data, size: 4096}
	hole := rewriteKindSource{kind: sparse.Hole, size: 4096}
	if err := compareSparseStreams(context.Background(), zero, data); err != nil {
		t.Fatalf("zero data must match explicit zero: %v", err)
	}
	if err := compareSparseStreams(context.Background(), zero, hole); err == nil {
		t.Fatal("hole incorrectly equivalent to zero")
	}
	data.err = io.ErrUnexpectedEOF
	if err := compareSparseStreams(context.Background(), zero, data); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("source error lost: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := compareSparseStreams(ctx, zero, zero); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestRewriteRejectsUnmatchedBeforeOutput(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	_, cfg := publishPortable(t, "")
	ref := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{3}, 4096)))
	parsed, _ := manifest.ParseRef(ref)
	options := RewriteOptions{Replacements: []RefReplacement{{Old: "file://missing@digest:" + publishTestSHA, New: "file://new@digest:" + publishTestSHA}}, SkipVerify: true}
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), options)
	if err == nil || result.Ref != "" {
		t.Fatalf("unmatched result=%+v error=%v", result, err)
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid plan created files: %v %v", entries, err)
	}
}

func TestRewriteSpecifiedDiskReduction(t *testing.T) {
	for _, mode := range []string{"automatic", "existing", "different"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			input, output := t.TempDir(), t.TempDir()
			sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
			storage, err := NewProcessStorage(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			publisher := rewriteTarget(t, storage, output)
			bottom := bytes.Repeat([]byte{0x31}, 8192)
			low, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bottom), nil)
			if err != nil {
				t.Fatal(err)
			}
			topBytes := bytes.Repeat([]byte{0x42}, 8192)
			top, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(topBytes), []sparse.Extent{{Offset: 4096, Size: 4096}})
			if err != nil {
				t.Fatal(err)
			}
			composite := append([]byte(nil), topBytes...)
			copy(composite[4096:], bottom[4096:])
			if mode == "different" {
				composite[5000] ^= 1
			}
			merged, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(composite), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, cfg := publishPortable(t, "")
			cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: top, BaseFromRefs: []string{low}}}}
			cfg.Mounts = []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
			ref := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{5}, 8192)))
			parsed, _ := manifest.ParseRef(ref)
			rule := RefReduction{Top: top}
			if mode != "automatic" {
				rule.Target = merged
			}
			result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{Reductions: []RefReduction{rule}})
			if mode == "different" {
				if err == nil || result.Ref != "" {
					t.Fatal("different reduction target accepted")
				}
				entries, _ := os.ReadDir(output)
				if len(entries) != 0 {
					t.Fatal("invalid equivalence wrote files")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if len(root.Portable.Boot.Disks[0].BaseFromRefs) != 0 {
				t.Fatal("disk lowers remain")
			}
			disk := rewriteOpen(t, publisher, root.Portable.Boot.Disks[0].Base)
			defer disk.Close()
			actual, err := readPublishSource(ctx, disk)
			if err != nil || !bytes.Equal(actual, composite) {
				t.Fatalf("wrong reduction: %v", err)
			}
		})
	}
}

func TestRewriteSnapshotUpdatesPortableSandboxInternals(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "missing-old-skip"}[skip], func(t *testing.T) {
			ctx := context.Background()
			input, output := t.TempDir(), t.TempDir()
			sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
			storage, err := NewProcessStorage(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			publisher := rewriteTarget(t, storage, output)
			publisher.locations["source"] = input
			body := bytes.Repeat([]byte{0x53}, 4096)
			old, oldPath, err := sink.AbsorbOverlay(ctx, bytes.NewReader(body), nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(oldPath)
			if err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(input, "new.overlay")
			if err = os.WriteFile(copyPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			oldParsed, _ := manifest.ParseRef(old)
			newRef := manifest.Ref{Scheme: "file", Path: copyPath, DigestScheme: oldParsed.DigestScheme, Digest: oldParsed.Digest}.String()
			oldParsed.Location = "source"
			old = oldParsed.String()
			_, cfg := publishPortable(t, old)
			eRef := rewriteSaveE(t, sink, cfg, rewriteSparse(t, body))
			eParsed, _ := manifest.ParseRef(eRef)
			eParsed.Location = "source"
			sRef := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eParsed.String()}, rewriteSparse(t, body))
			sParsed, _ := manifest.ParseRef(sRef)
			if skip {
				if err = os.Remove(oldPath); err != nil {
					t.Fatal(err)
				}
			}
			result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, sParsed.Path), RewriteOptions{Replacements: []RefReplacement{{Old: old, New: newRef}}, SkipVerify: skip})
			if err != nil {
				t.Fatal(err)
			}
			s, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			scfg, err := snapshot.ParseConfig(s.SnapshotConfig)
			if err != nil {
				t.Fatal(err)
			}
			if scfg.SandboxRef == eParsed.String() {
				t.Fatal("portable E was not rewritten")
			}
			e, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, scfg.SandboxRef))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if len(e.Portable.Boot.Root.BaseFromRefs) != 1 || e.Portable.Boot.Root.BaseFromRefs[0] == old {
				t.Fatal("E internal ref not replaced")
			}
		})
	}
}

func TestRewriteReplacementPreservesUnreducedTop(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	lower := bytes.Repeat([]byte{0x33}, 8192)
	old, path, err := sink.AbsorbOverlay(ctx, bytes.NewReader(lower), nil)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(input, "replacement.overlay")
	if err = os.WriteFile(copyPath, physical, 0600); err != nil {
		t.Fatal(err)
	}
	parsed, _ := manifest.ParseRef(old)
	parsed.Path = copyPath
	_, cfg := publishPortable(t, old)
	top := bytes.Repeat([]byte{0x44}, 8192)
	ref := rewriteSaveE(t, sink, cfg, rewriteSparse(t, top, sparse.Extent{Offset: 4096, Size: 4096}))
	inputRef, _ := manifest.ParseRef(ref)
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, inputRef.Path), RewriteOptions{Replacements: []RefReplacement{{Old: old, New: parsed.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if len(root.Portable.Boot.Root.BaseFromRefs) != 1 {
		t.Fatal("replacement changed chain length")
	}
	run, err := root.Payload.RunAt(4096, 4096)
	if err != nil || run.Kind() != sparse.Hole {
		t.Fatalf("replacement materialized lower content: %v %v", run, err)
	}
}

func TestRewriteSkipKeepsInputIdentityValidation(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	bytesValue := bytes.Repeat([]byte{0x73}, 4096)
	old, path, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytesValue), nil)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	position := bytes.Index(physical, bytesValue[:64])
	if position < 0 {
		t.Fatal("fixture payload missing")
	}
	physical[position] ^= 1
	copyPath := filepath.Join(input, "corrupt.overlay")
	if err = os.WriteFile(copyPath, physical, 0600); err != nil {
		t.Fatal(err)
	}
	newRef, _ := manifest.ParseRef(old)
	newRef.Path = copyPath
	_, cfg := publishPortable(t, old)
	rootRef := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytesValue))
	parsed, _ := manifest.ParseRef(rootRef)
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{Replacements: []RefReplacement{{Old: old, New: newRef.String()}}, SkipVerify: true})
	if err == nil || result.Ref != "" {
		t.Fatal("skip-verify-ref bypassed input identity")
	}
	entries, _ := os.ReadDir(output)
	if len(entries) != 0 {
		t.Fatal("corrupt identity produced output")
	}
}

func TestRewriteExplicitMemoryReductionPreservesExecutionState(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	_, cfg := publishPortable(t, "")
	eRef := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{0x20}, 8192)))
	parentBytes := bytes.Repeat([]byte{0x31}, 8192)
	parent := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef}, rewriteSparse(t, parentBytes))
	top := bytes.Repeat([]byte{0x42}, 8192)
	source := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: []string{parent}}, rewriteSparse(t, top, sparse.Extent{Offset: 4096, Size: 4096}))
	composite := append([]byte(nil), top...)
	copy(composite[4096:], parentBytes[4096:])
	merged, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(composite), nil)
	if err != nil {
		t.Fatal(err)
	}
	inputRef, _ := manifest.ParseRef(source)
	inputRef.Path = filepath.Join(input, inputRef.Path)
	target, _ := manifest.ParseRef(merged)
	target.Path = filepath.Join(input, target.Path)
	result, err := publisher.PublishWithOptions(ctx, inputRef.String(), RewriteOptions{Reductions: []RefReduction{{Top: inputRef.String(), Target: target.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	sCfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil || len(sCfg.FromRefs) != 0 {
		t.Fatalf("memory chain retained: %v %v", sCfg, err)
	}
	actual, err := readPublishSource(ctx, root.Memory)
	if err != nil || !bytes.Equal(actual, composite) {
		t.Fatalf("memory reduction wrong: %v", err)
	}
	if string(root.StateJSON) != `{"ticks":42}` {
		t.Fatal("execution state changed")
	}
}

func TestRewriteBundleDependenciesSurviveRemovedSource(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	input, output := t.TempDir(), t.TempDir()
	sink, err := snapshot.NewPlannedBundleSink(input, "bundle", cfg, storage.CustomerKeyFunc(), admission, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := bytes.Repeat([]byte{0x36}, 8192)
	disk, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(diskBytes), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, runtime := publishPortable(t, "")
	runtime.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: disk}}}
	runtime.Mounts = []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	runtimeBytes, err := config.MarshalPortableSandboxConfig(runtime)
	if err != nil {
		t.Fatal(err)
	}
	esource, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x42}, 8192)), nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	eref, _, err := sink.AbsorbSandbox(ctx, esource)
	if err != nil {
		t.Fatal(err)
	}
	scfg, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eref})
	if err != nil {
		t.Fatal(err)
	}
	ssource, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x57}, 8192)), []byte("{}"), []byte("{}"), scfg)
	if err != nil {
		t.Fatal(err)
	}
	sref, _, err := sink.AbsorbSnapshot(ctx, ssource)
	if err != nil {
		t.Fatal(err)
	}
	if err = sink.CommitSnapshot(ctx, sref, ""); err != nil {
		t.Fatal(err)
	}
	if err = sink.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := manifest.ParseKeyRef(sref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(input, manifest.HexKey(key)+".bundle")
	publisher := rewriteTarget(t, storage, output)
	result, err := publisher.PublishWithOptions(ctx, path, RewriteOptions{ReduceAny: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(input); err != nil {
		t.Fatal(err)
	}
	s, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sconfig, err := snapshot.ParseConfig(s.SnapshotConfig)
	if err != nil {
		t.Fatal(err)
	}
	e, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, sconfig.SandboxRef))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	publishedDisk := e.Portable.Boot.Disks[0].Base
	if publishedDisk == disk {
		t.Fatal("Bundle-scoped Manifest escaped unbound")
	}
	opened := rewriteOpen(t, publisher, publishedDisk)
	defer opened.Close()
	actual, err := readPublishSource(ctx, opened)
	if err != nil || !bytes.Equal(actual, diskBytes) {
		t.Fatalf("source-independent data: %v", err)
	}
}

func TestRewriteExplicitReducedTargetRepairsMissingChain(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	old := "file://unavailable-top.overlay@digest:" + publishTestSHA
	lower := "file://unavailable-low.overlay@digest:" + publishTestSHA
	newRef, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{0x37}, 4096)), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := publishPortable(t, "")
	cfg.Boot.Disks = []config.PortableDiskConfig{{Name: "data", PortableRootConfig: config.PortableRootConfig{Base: old, BaseFromRefs: []string{lower}}}}
	cfg.Mounts = []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	eRef := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{0x28}, 4096)))
	parsed, _ := manifest.ParseRef(eRef)
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{Reductions: []RefReduction{{Top: old, Target: newRef}}, SkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	e, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if len(e.Portable.Boot.Disks[0].BaseFromRefs) != 0 {
		t.Fatal("repaired chain retains missing refs")
	}
}

func TestRewriteSandboxReferenceUsesSemanticDiskEquivalence(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "equivalent", true: "topology-changed"}[changed], func(t *testing.T) {
			ctx := context.Background()
			input, output := t.TempDir(), t.TempDir()
			sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
			storage, err := NewProcessStorage(nil)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			publisher := rewriteTarget(t, storage, output)
			bottom := bytes.Repeat([]byte{0x31}, 8192)
			lower, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bottom), nil)
			if err != nil {
				t.Fatal(err)
			}
			top := bytes.Repeat([]byte{0x42}, 8192)
			_, cfg := publishPortable(t, lower)
			oldE := rewriteSaveE(t, sink, cfg, rewriteSparse(t, top, sparse.Extent{Offset: 4096, Size: 4096}))
			composite := append([]byte(nil), top...)
			copy(composite[4096:], bottom[4096:])
			_, newCfg := publishPortable(t, "")
			if changed {
				newCfg.Launch.Workdir = "/tmp"
			}
			newE := rewriteSaveE(t, sink, newCfg, rewriteSparse(t, composite))
			sRef := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: oldE}, rewriteSparse(t, top))
			parsed, _ := manifest.ParseRef(sRef)
			result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{Replacements: []RefReplacement{{Old: oldE, New: newE}}})
			if changed {
				if err == nil || result.Ref != "" {
					t.Fatal("non-reference config change accepted")
				}
				entries, _ := os.ReadDir(output)
				if len(entries) != 0 {
					t.Fatal("invalid replacement emitted artifacts")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			s, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			sCfg, err := snapshot.ParseConfig(s.SnapshotConfig)
			if err != nil {
				t.Fatal(err)
			}
			e, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, sCfg.SandboxRef))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			data, err := readPublishSource(ctx, e.Payload)
			if err != nil || !bytes.Equal(data, composite) {
				t.Fatalf("different effective disk: %v", err)
			}
		})
	}
}

func TestRewriteReduceSixtyFourMemoryLayers(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	_, cfg := publishPortable(t, "")
	eRef := rewriteSaveE(t, sink, cfg, rewriteSparse(t, bytes.Repeat([]byte{0x21}, 4096)))
	parents := make([]string, 64)
	for i := range parents {
		parents[i] = rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: "file://historical.sandbox@digest:" + publishTestSHA}, rewriteSparse(t, bytes.Repeat([]byte{byte(0x40 + i)}, 4096)))
	}
	sRef := rewriteSaveS(t, sink, &snapshot.Config{Version: 1, SandboxRef: eRef, FromRefs: parents}, rewriteSparse(t, make([]byte, 4096), sparse.Extent{Offset: 0, Size: 4096}))
	parsed, _ := manifest.ParseRef(sRef)
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{ReduceAny: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := snapshotfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	data, err := readPublishSource(ctx, s.Memory)
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte{0x40}, 4096)) {
		t.Fatalf("wrong top-first memory: %v", err)
	}
	repeated, err := publisher.PublishWithOptions(ctx, result.Ref, RewriteOptions{ReduceAny: true})
	if err != nil || repeated.Ref != result.Ref {
		t.Fatalf("reduction is not idempotent: %v %v", repeated, err)
	}
}

func TestRewriteReduceEROFSKeepsImmutableBaseSeparate(t *testing.T) {
	ctx := context.Background()
	input, output := t.TempDir(), t.TempDir()
	sink := snapshot.NewFileSink(input, "fixture", nil, false, nil)
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := rewriteTarget(t, storage, output)
	imagePayload := publishEROFSFixture()
	imageRef, _, err := sink.AbsorbImageSource(ctx, publishSource(t, imagePayload))
	if err != nil {
		t.Fatal(err)
	}
	bottom := bytes.Repeat([]byte{0x33}, 8192)
	lower, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bottom), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := publishPortable(t, "")
	cfg.Boot.Root = config.PortableRootConfig{Base: imageRef, Overlay: &config.PortableOverlayConfig{Base: "self", BaseFromRefs: []string{lower}}}
	top := bytes.Repeat([]byte{0x44}, 8192)
	ref := rewriteSaveE(t, sink, cfg, rewriteSparse(t, top, sparse.Extent{Offset: 4096, Size: 4096}))
	parsed, _ := manifest.ParseRef(ref)
	result, err := publisher.PublishWithOptions(ctx, filepath.Join(input, parsed.Path), RewriteOptions{ReduceAny: true})
	if err != nil {
		t.Fatal(err)
	}
	e, err := sandboxfile.Open(ctx, rewriteOpen(t, publisher, result.Ref))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Portable.Boot.Root.Base == "self" || e.Portable.Boot.Root.Overlay == nil || e.Portable.Boot.Root.Overlay.Base != "self" || len(e.Portable.Boot.Root.Overlay.BaseFromRefs) != 0 {
		t.Fatal("EROFS/upper topology changed")
	}
	want := append([]byte(nil), top...)
	copy(want[4096:], bottom[4096:])
	got, err := readPublishSource(ctx, e.Payload)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("upper merge: %v", err)
	}
	immutable := rewriteOpen(t, publisher, e.Portable.Boot.Root.Base)
	defer immutable.Close()
	got, err = readPublishSource(ctx, immutable)
	if err != nil || !bytes.Equal(got, imagePayload) {
		t.Fatalf("immutable base changed: %v", err)
	}
}
