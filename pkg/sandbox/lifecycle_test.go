package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/guestlink"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

type lifecycleArtifactStream struct{ sparse.Source }

func (*lifecycleArtifactStream) Close() error { return nil }

func TestApplyRunCaptureSourcesForwardsBundleFetcher(t *testing.T) {
	reader := new(manifestbundle.Reader)
	fetcher := new(manifestbundle.ManifestFetcher)
	opts := RunOptions{BundleReader: reader, BundleFetcher: fetcher}
	params := new(VMParams)
	applyRunCaptureSources(params, opts)
	if params.BundleReader != reader || params.BundleFetcher != fetcher {
		t.Fatalf("capture Bundle sources = reader %p fetcher %p, want %p/%p",
			params.BundleReader, params.BundleFetcher, reader, fetcher)
	}
}

func lifecycleSnapshotSource(t testing.TB, memory, snapshotConfig []byte) sparse.Source {
	t.Helper()
	logical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(memory), uint64(len(memory))),
		[]byte("{}"), []byte("{}"), snapshotConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	return logical
}

func TestEnsureSnapshotDirRejectsSymlinkBeforeCapture(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "output")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureSnapshotDir(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ensureSnapshotDir error = %v, want symlink rejection", err)
	}
}

func TestPrepareSnapshotDependencyRejectsLiveSandboxAsRootImage(t *testing.T) {
	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096)), 4096),
		nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = prepareSnapshotDependencyStream(
		context.Background(), &lifecycleArtifactStream{Source: logical}, dependencyRootImage,
	)
	if err == nil || !strings.Contains(err.Error(), "EROFS") {
		t.Fatalf("root-image dependency error = %v", err)
	}
}

func TestPreflightWritableExt4RejectsUnformattedExplicitDiff(t *testing.T) {
	dir := t.TempDir()
	diffPath := filepath.Join(dir, "upper.diff")
	if err := os.WriteFile(diffPath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	root := &config.RootConfig{
		Base: "file:///unused.erofs",
		Overlay: &config.OverlayConfig{
			Diff: "file://" + diffPath,
		},
	}
	err := preflightWritableExt4(context.Background(), root, "boot.root", "",
		nil, nil, nil, [32]byte{}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "formatted ext4") {
		t.Fatalf("unformatted active diff error = %v", err)
	}
}

func lifecycleBundleSnapshot(t testing.TB, sink *snapshot.BundleSink, directory string, memory, snapshotConfig []byte) (string, string) {
	t.Helper()
	ref, _, err := sink.AbsorbSnapshot(context.Background(), lifecycleSnapshotSource(t, memory, snapshotConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(context.Background(), ref, ""); err != nil {
		t.Fatal(err)
	}
	key, err := manifest.ParseKeyRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	return ref, filepath.Join(directory, manifest.HexKey(key)+".bundle")
}

func TestCanonicalBundleSourceNormalizesLocatedAlias(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("b", 64)
	bundleName := key + ".bundle"
	bundlePath := filepath.Join(dir, bundleName)
	if err := os.WriteFile(bundlePath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(dir, "lower.snapshot")
	if err := os.Symlink(bundleName, aliasPath); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef("file://lower.snapshot@manifest:" + key + "@location:A")
	if err != nil {
		t.Fatal(err)
	}

	got, real, err := canonicalBundleSource(aliasPath, ref)
	if err != nil {
		t.Fatal(err)
	}
	if want := "file://" + bundleName + "@location:A"; got != want {
		t.Fatalf("source = %q, want %q", got, want)
	}
	if real != bundlePath {
		t.Fatalf("real path = %q, want %q", real, bundlePath)
	}

	noncanonicalPath := filepath.Join(dir, "regular.snapshot")
	if err := os.WriteFile(noncanonicalPath, []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref.Path = filepath.Base(noncanonicalPath)
	if _, _, err := canonicalBundleSource(noncanonicalPath, ref); err == nil {
		t.Fatal("noncanonical located Bundle source was accepted")
	}
}

func TestNormalizeLocalMemoryRefsEmitsSiblingBasenames(t *testing.T) {
	digest := strings.Repeat("b", 64)
	locatedRef := "file://located.snapshot@sha256:" + digest + "@location:parent"
	manifestRef := "manifest://" + strings.Repeat("a", 64)
	got, err := normalizeLocalMemoryRefs([]string{
		"file:///legacy/path/base.snapshot",
		"file://nested/parent.snapshot",
		locatedRef,
		manifestRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"file://base.snapshot", "file://parent.snapshot", locatedRef, manifestRef}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("normalized refs = %v, want %v", got, want)
	}

	if _, err := normalizeLocalMemoryRefs([]string{"file://.."}); err == nil {
		t.Fatal("normalizeLocalMemoryRefs(file://..) succeeded")
	}
}

func TestSnapshotMemoryRefsThreeGenerationWorkingSetChain(t *testing.T) {
	portable := "manifest://" + strings.Repeat("a", 64)
	wDigest := strings.Repeat("b", 64)
	bDigest := strings.Repeat("c", 64)
	binding := &MemorySourceBinding{
		SnapshotRef: "file:///bundle/w.snapshot@sha256:" + wDigest,
		FromRefs: []string{
			"file:///bundle/b.snapshot@sha256:" + bDigest,
			portable,
		},
	}

	merged, err := memoryRefsForSnapshot(binding, true)
	if err != nil {
		t.Fatal(err)
	}
	wantMerged := []string{"file://b.snapshot@sha256:" + bDigest, portable}
	if strings.Join(merged, ",") != strings.Join(wantMerged, ",") {
		t.Fatalf("merged memory refs = %v, want %v", merged, wantMerged)
	}

	workingSet, err := memoryRefsForSnapshot(binding, false)
	if err != nil {
		t.Fatal(err)
	}
	wantWorkingSet := []string{
		"file://w.snapshot@sha256:" + wDigest,
		"file://b.snapshot@sha256:" + bDigest,
		portable,
	}
	if strings.Join(workingSet, ",") != strings.Join(wantWorkingSet, ",") {
		t.Fatalf("working-set memory refs = %v, want %v", workingSet, wantWorkingSet)
	}

	if binding.FromRefs[0] != "file:///bundle/b.snapshot@sha256:"+bDigest {
		t.Fatalf("memoryRefsForSnapshot mutated binding: %v", binding.FromRefs)
	}
}

func TestSnapshotMemoryRefsRejectsProspectiveChainOverLimit(t *testing.T) {
	binding := &MemorySourceBinding{
		SnapshotRef: "manifest://" + strings.Repeat("a", 64),
		FromRefs:    make([]string, snapshot.MaxMemoryFromRefs),
	}
	for i := range binding.FromRefs {
		binding.FromRefs[i] = fmt.Sprintf("file://%064x.snapshot@sha256:%064x", i+1, i+1)
	}

	if _, err := memoryRefsForSnapshot(binding, false); err == nil || !strings.Contains(err.Error(), "exceeds 64 entries") {
		t.Fatalf("unmerged memory chain error = %v", err)
	}
	if refs, err := memoryRefsForSnapshot(binding, true); err != nil || len(refs) != snapshot.MaxMemoryFromRefs {
		t.Fatalf("merged memory chain = %d refs, %v", len(refs), err)
	}
}

func TestPrepareSnapshotBundlePlanCopiesReachableParentManifest(t *testing.T) {
	customerKey := [32]byte{0x71, 0x72, 0x73}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	configFor := func(generation string) *manifest.Config {
		return &manifest.Config{
			Manifest: manifest.ManifestSubConfig{WriteGeneration: generation},
			Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
			Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
		}
	}
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://" + strings.Repeat("a", 64) + "\n")
	parentDir := t.TempDir()
	parentCfg := configFor("G1")
	parent, err := snapshot.NewBundleSink(context.Background(), parentDir, "parent", parentCfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = parent.AbsorbOverlay(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x30}, 8192)), nil)
	if err != nil {
		t.Fatal(err)
	}
	parentRef, parentPath := lifecycleBundleSnapshot(t, parent, parentDir,
		bytes.Repeat([]byte{0x31}, 8192), snapshotConfig)
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	parentKey, err := manifest.ParseKeyRef(parentRef)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := manifest.ParseRef("file://" + parentPath + "@manifest:" + manifest.HexKey(parentKey))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := artifact.OpenFile(context.Background(), parentPath, selector,
		parentCfg, keyFn, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	childDir := t.TempDir()
	childCfg := configFor("G1")
	admission, err := childCfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sandboxCfg := &config.SandboxConfig{}
	sandboxCfg.Resources.Capacity.Memory = "8KiB"
	sandboxCfg.Boot.Root.Overlay = &config.OverlayConfig{}
	runOpts := RunOptions{
		Cfg: sandboxCfg,
		PortableConfig: &config.PortableSandboxConfig{Boot: config.PortableBootConfig{
			Root: config.PortableRootConfig{Base: "self"},
		}},
		MemoryBinding: &MemorySourceBinding{
			SnapshotRef: parentRef,
			BundleSource: &BundleSourceBinding{
				RootRef: "file://" + filepath.Base(parentPath), RootPath: parentPath,
			},
		},
		ManifestCfg:   childCfg,
		Fetcher:       opened.ScopedFetcher(),
		BundleReader:  opened.BundleReader(),
		BundleFetcher: opened.ManifestFetcher(),
		CustomerKeyFn: keyFn,
	}
	rootSource, err := opened.ManifestFetcher().SelectRoot(parentKey)
	if err != nil {
		t.Fatal(err)
	}
	remote := &failingManifestFetcher{}
	remoteOpts := runOpts
	remoteOpts.BundleFetcher = manifestbundle.NewManifestFetcher(opened.BundleReader(), rootSource.Fetcher, remote)
	if _, _, _, err := bundleSourceForManifest(context.Background(), store.ContentKey{0xff}, remoteOpts); err == nil || remote.calls != 1 {
		t.Fatalf("remote source confirmation error = %v, calls=%d", err, remote.calls)
	}
	plan, err := prepareSnapshotBundlePlan(context.Background(), runOpts,
		[]string{parentRef}, []bool{false}, childDir, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if got := plan.Refs(); len(got) != 0 {
		t.Fatalf("fully materialized plan has external refs: %v", got)
	}
	child, err := snapshot.NewPlannedBundleSink(childDir, "child", childCfg, keyFn, admission, plan.Refs(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	replacements, err := plan.Ingest(context.Background(), child, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacements[parentRef] != parentRef {
		t.Fatalf("exact-copy replacement = %q, want %q", replacements[parentRef], parentRef)
	}
	_, childPath := lifecycleBundleSnapshot(t, child, childDir,
		bytes.Repeat([]byte{0x41}, 8192), snapshotConfig)
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := manifestbundle.Open(childPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got := reader.Refs(); len(got) != 0 {
		t.Fatalf("child retained external refs: %v", got)
	}
	if !reader.HasManifest(parentKey) {
		t.Fatal("child did not copy the reachable parent Manifest")
	}

	mergedPlan, err := prepareSnapshotBundlePlan(context.Background(), runOpts,
		nil, []bool{true}, t.TempDir(), admission)
	if err != nil {
		t.Fatal(err)
	}
	defer mergedPlan.Close()
	if refs := mergedPlan.Refs(); len(refs) != 0 {
		t.Fatalf("merged and unreachable parent source remained in refs: %v", refs)
	}
}

type failingManifestFetcher struct {
	calls int
}

func (f *failingManifestFetcher) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	f.calls++
	return nil, errors.New("remote Manifest missing")
}

func TestPrepareSnapshotBundlePlanMaterializesOnlyReachableSources(t *testing.T) {
	customerKey := [32]byte{0x74, 0x75, 0x76}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	manifestCfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	makeBundle := func(directory, sid string, refs []string, fill byte, withOverlay bool) (string, string, string) {
		t.Helper()
		admission, err := manifestCfg.WriteAdmission(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sink, err := snapshot.NewPlannedBundleSink(directory, sid, manifestCfg, keyFn, admission, refs, nil)
		if err != nil {
			t.Fatal(err)
		}
		var overlayRef string
		if withOverlay {
			overlayRef, _, err = sink.AbsorbOverlay(context.Background(), bytes.NewReader(bytes.Repeat([]byte{fill}, 8192)), nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		rootRef, path := lifecycleBundleSnapshot(t, sink, directory,
			bytes.Repeat([]byte{fill + 1}, 8192),
			[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("a", 64)+"\n"))
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		return rootRef, path, overlayRef
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	_, pathA, _ := makeBundle(dirA, "unused-a", nil, 0x21, false)
	_, pathB, layerB := makeBundle(dirB, "used-b", nil, 0x31, true)
	refA := "file://" + filepath.Base(pathA) + "@location:A"
	refB := "file://" + filepath.Base(pathB) + "@location:B"
	parentDir := t.TempDir()
	parentRoot, parentPath, _ := makeBundle(parentDir, "parent", []string{refA, refB}, 0x41, false)
	parentPhysical := "file://" + filepath.Base(parentPath)
	locations := config.RefLocations{"A": dirA, "B": dirB}
	selector, err := manifest.ParseRef("file://" + parentPath + "@manifest:" + strings.TrimPrefix(parentRoot, "manifest://"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := artifact.OpenFileWithLocations(context.Background(), parentPath, selector,
		manifestCfg, keyFn, nil, locations, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	sandboxCfg := &config.SandboxConfig{}
	sandboxCfg.Resources.Capacity.Memory = "8KiB"
	sandboxCfg.Boot.Root.Overlay = &config.OverlayConfig{}
	childCfg := *manifestCfg
	childCfg.Manifest.WriteGeneration = "G2"
	admission, err := childCfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	childDir := t.TempDir()
	plan, err := prepareSnapshotBundlePlan(context.Background(), RunOptions{
		Cfg: sandboxCfg,
		PortableConfig: &config.PortableSandboxConfig{Boot: config.PortableBootConfig{
			Root: config.PortableRootConfig{Base: "self", BaseFromRefs: []string{layerB}},
		}},
		MemoryBinding: &MemorySourceBinding{
			SnapshotRef: parentRoot,
			BundleSource: &BundleSourceBinding{
				RootRef: parentPhysical, RootPath: parentPath, Refs: opened.BundleReader().Refs(),
			},
		},
		ManifestCfg: &childCfg, Fetcher: opened.ScopedFetcher(),
		BundleReader: opened.BundleReader(), BundleFetcher: opened.ManifestFetcher(),
		RefLocations: locations, CustomerKeyFn: keyFn,
	}, []string{parentRoot}, []bool{false}, childDir, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if got := plan.Refs(); len(got) != 0 {
		t.Fatalf("materialized plan has external refs: %v", got)
	}
	sink, err := snapshot.NewPlannedBundleSink(childDir, "materialized", &childCfg, keyFn, admission, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	replacements, err := plan.Ingest(context.Background(), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacements[parentRoot] == "" || replacements[layerB] == "" {
		t.Fatalf("reachable dependencies were not materialized: %v", replacements)
	}
	if _, retained := replacements[refA]; retained {
		t.Fatal("unreachable historical Bundle source was materialized")
	}
}

func TestBundleMemoryMergeUsesSnapshotBundleProvenance(t *testing.T) {
	ctx := context.Background()
	customerKey := [32]byte{0x45, 0x46, 0x47}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	manifestCfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://" + strings.Repeat("a", 64) + "\n")

	makeBundle := func(sid string, fill byte) (string, string, *artifact.OpenedFile) {
		t.Helper()
		dir := t.TempDir()
		sink, err := snapshot.NewBundleSink(ctx, dir, sid, manifestCfg, keyFn, nil)
		if err != nil {
			t.Fatal(err)
		}
		ref, path := lifecycleBundleSnapshot(t, sink, dir, bytes.Repeat([]byte{fill}, 4096), snapshotConfig)
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
		key, err := manifest.ParseKeyRef(ref)
		if err != nil {
			t.Fatal(err)
		}
		selector, err := manifest.ParseRef("file://" + path + "@manifest:" + manifest.HexKey(key))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := artifact.OpenFile(ctx, path, selector, manifestCfg, keyFn, nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		return ref, path, opened
	}

	sandboxRef, sandboxPath, sandboxOpened := makeBundle("separate-e", 0x51)
	defer sandboxOpened.Close()
	snapshotRef, snapshotPath, snapshotOpened := makeBundle("separate-s", 0x61)
	defer snapshotOpened.Close()

	binding := func(path string, opened *artifact.OpenedFile) *BundleSourceBinding {
		return &BundleSourceBinding{
			RootRef: "file://" + filepath.Base(path), RootPath: path, Refs: opened.BundleReader().Refs(),
			Reader: opened.BundleReader(), Fetcher: opened.ManifestFetcher(),
		}
	}
	opts := RunOptions{
		SourceBinding: &RunSourceBinding{
			SandboxRef: sandboxRef, BundleSource: binding(sandboxPath, sandboxOpened),
		},
		MemoryBinding: &MemorySourceBinding{
			SnapshotRef: snapshotRef, BundleSource: binding(snapshotPath, snapshotOpened),
		},
		// Reproduce restore's historical single-selector preference for E.
		BundleReader: sandboxOpened.BundleReader(), BundleFetcher: sandboxOpened.ManifestFetcher(),
	}

	resolved, merged, err := bundleMemoryManifestMergeRef(ctx, snapshotRef, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !merged {
		t.Fatal("Snapshot Bundle memory parent was not selected for merge")
	}
	ref, err := manifest.ParseRef(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(ref.Path) != filepath.Clean(snapshotPath) {
		t.Fatalf("memory merge source path = %q, want Snapshot Bundle %q", ref.Path, snapshotPath)
	}
	resolved, merged, err = bundleManifestMergeRef(ctx, sandboxRef, opts)
	if err != nil || !merged {
		t.Fatalf("Sandbox Bundle disk parent merge = %q, %t, %v", resolved, merged, err)
	}
	ref, err = manifest.ParseRef(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(ref.Path) != filepath.Clean(sandboxPath) {
		t.Fatalf("disk merge source path = %q, want Sandbox Bundle %q", ref.Path, sandboxPath)
	}
	for _, dependency := range []struct {
		name   string
		value  artifactDependency
		reader *manifestbundle.Reader
	}{
		{name: "memory", value: artifactDependency{raw: snapshotRef, role: dependencyMemorySnapshot}, reader: snapshotOpened.BundleReader()},
		{name: "disk", value: artifactDependency{raw: sandboxRef, role: dependencyDiskLayer}, reader: sandboxOpened.BundleReader()},
	} {
		t.Run(dependency.name+" dependency selector", func(t *testing.T) {
			stream, reader, _, _, err := openSnapshotDependency(ctx, dependency.value, opts, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if reader != dependency.reader {
				t.Fatalf("selected Bundle reader = %p, want %p", reader, dependency.reader)
			}
		})
	}
}

func TestPrepareSnapshotBundlePlanIngestsTarstreamParentIntoCurrentAdmission(t *testing.T) {
	parentDir := t.TempDir()
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://" + strings.Repeat("a", 64) + "\n")
	parentRef, parentPath, err := snapshot.NewFileSink(parentDir, "tar-parent", nil, false, nil).AbsorbSnapshot(
		context.Background(), lifecycleSnapshotSource(t, bytes.Repeat([]byte{0x56}, 8192), snapshotConfig))
	if err != nil {
		t.Fatal(err)
	}
	customerKey := [32]byte{0x77, 0x78, 0x79}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	manifestCfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G2"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	admission, err := manifestCfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sandboxCfg := &config.SandboxConfig{}
	sandboxCfg.Resources.Capacity.Memory = "8KiB"
	sandboxCfg.Boot.Root.Overlay = &config.OverlayConfig{}
	outputDir := t.TempDir()
	plan, err := prepareSnapshotBundlePlan(context.Background(), RunOptions{
		Cfg: sandboxCfg,
		PortableConfig: &config.PortableSandboxConfig{Boot: config.PortableBootConfig{
			Root: config.PortableRootConfig{Base: "self"},
		}},
		MemoryBinding: &MemorySourceBinding{
			SnapshotRef: parentRef, RuntimeRef: parentRef, RelativeDir: filepath.Dir(parentPath),
		},
		ManifestCfg: manifestCfg, CustomerKeyFn: keyFn,
	}, []string{parentRef}, []bool{false}, outputDir, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	if refs := plan.Refs(); len(refs) != 0 {
		t.Fatalf("tarstream parent leaked into bundle/refs: %v", refs)
	}
	sink, err := snapshot.NewPlannedBundleSink(outputDir, "tar-child", manifestCfg, keyFn, admission, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	replacements, err := plan.Ingest(context.Background(), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	importedRef := replacements[parentRef]
	importedKey, err := manifest.ParseKeyRef(importedRef)
	if err != nil {
		t.Fatalf("tarstream replacement = %q: %v", importedRef, err)
	}
	_, childPath := lifecycleBundleSnapshot(t, sink, outputDir,
		bytes.Repeat([]byte{0x57}, 8192), snapshotConfig)
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := manifestbundle.Open(childPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Admission() != admission || !reader.HasManifest(importedKey) {
		t.Fatalf("tar parent was not ingested under current admission %v", admission)
	}
}

func TestColdDiskOpenerAutoDetectsBundleSelector(t *testing.T) {
	customerKey := [32]byte{0x91, 0x92, 0x93}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	manifestCfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	directory := t.TempDir()
	sink, err := snapshot.NewBundleSink(context.Background(), directory, "cold-disk", manifestCfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := bytes.Repeat([]byte{0x67}, 8192)
	diskRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(diskBytes), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, bundlePath := lifecycleBundleSnapshot(t, sink, directory, make([]byte, 8192),
		[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("a", 64)+"\n"))
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	diskKey, err := manifest.ParseKeyRef(diskRef)
	if err != nil {
		t.Fatal(err)
	}
	selector := "file://" + bundlePath + "@manifest:" + manifest.HexKey(diskKey)
	opener := FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		return artifact.OpenFile(ctx, path, ref, manifestCfg, keyFn, nil, nil, false)
	})

	stream, size, err := OpenDiskStreamAtWithOpener(context.Background(), selector, nil, nil, "", nil, false, opener)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if size != int64(len(diskBytes)) {
		t.Fatalf("disk size = %d, want %d", size, len(diskBytes))
	}
	got := make([]byte, len(diskBytes))
	if n, err := stream.ReadAt(context.Background(), got, 0); err != nil || n != len(got) {
		t.Fatalf("Bundle disk ReadAt = %d, %v", n, err)
	}
	if !bytes.Equal(got, diskBytes) {
		t.Fatal("Bundle disk plaintext changed")
	}
	canonical, err := buildDiskRefWithOpener(selector, nil, nil, false, opener)
	if err != nil {
		t.Fatal(err)
	}
	want := "file://" + filepath.Base(bundlePath) + "@manifest:" + manifest.HexKey(diskKey)
	if canonical != want {
		t.Fatalf("snapshot disk ref = %q, want %q", canonical, want)
	}
}

func TestHandleSnapshotRequestRejectsMissingLocalMemoryLowerBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	parentCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version:    snapshot.SnapshotConfigVersion,
		SandboxRef: "manifest://" + strings.Repeat("9", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	parentSource, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096),
		[]byte("{}"), []byte("{}"), parentCfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	parentRef, parentPath, err := snapshot.NewFileSink(dir, "parent", nil, false, nil).
		AbsorbSnapshot(context.Background(), parentSource)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{}
	mfd, err := memory.Create("portable-chain-preflight", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	viewCalled := false
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(dir, "must-not-dial.sock")},
	}

	_, err = handleSnapshotRequest(context.Background(), ctl.Request{Upload: true}, RunOptions{
		Cfg:            cfg,
		PortableConfig: snapshotTestLivePortable(t),
		MemoryBinding: &MemorySourceBinding{
			SnapshotRef: parentRef, RuntimeRef: parentRef, RelativeDir: filepath.Dir(parentPath),
			FromRefs: []string{"file://base.snapshot@sha256:" + strings.Repeat("a", 64)},
		},
		SandboxID:   "test",
		ManifestCfg: &config.ManifestConfig{Store: manifest.StoreConfig{Endpoint: "unused"}},
	}, mfd, []SnapDiskRef{{
		DiffPath: filepath.Join(dir, "diff"),
		Size:     4096,
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalled = true
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		},
	}}, nil, filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"), "", nil, nil, pinger, nil, func() error { return nil }, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "open memory Snapshot file://base.snapshot@sha256:") {
		t.Fatalf("local lower preflight error = %v", err)
	}
	if viewCalled {
		t.Fatal("snapshot view opened before final memory-chain validation")
	}
}

func TestHandleSnapshotRequestRejectsMemoryRefCollisionBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	parentCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version:    snapshot.SnapshotConfigVersion,
		SandboxRef: "manifest://" + strings.Repeat("9", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	parentRef, parentPath, err := snapshot.NewFileSink(dir, "parent", nil, false, nil).
		AbsorbSnapshot(context.Background(), lifecycleSnapshotSource(t, make([]byte, 4096), parentCfg))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(parentRef)
	if err != nil {
		t.Fatal(err)
	}
	aliasRef := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.Link(parentPath, path); err != nil {
			t.Fatal(err)
		}
		ref := parsed
		ref.Path = name
		return ref.String()
	}
	firstRef := aliasRef("first.snapshot")
	secondRef := aliasRef("second.snapshot")

	mfd, err := memory.Create("memory-ref-collision", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	viewCalled := false
	mergeRef := false
	forwarder := NewForwarder("", discardLogf)
	_, err = handleSnapshotRequest(context.Background(), ctl.Request{
		OutDir: filepath.Join(dir, "out"), MergeRef: &mergeRef,
	}, RunOptions{
		Cfg: &config.SandboxConfig{}, PortableConfig: snapshotTestLivePortable(t), SandboxID: "collision",
		MemoryBinding: &MemorySourceBinding{
			SnapshotRef: firstRef, FromRefs: []string{secondRef}, RelativeDir: dir,
		},
	}, mfd, []SnapDiskRef{{
		DiffPath: filepath.Join(dir, "active.diff"), Size: 4096,
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalled = true
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		},
	}}, nil, filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"),
		"", nil, nil, nil, forwarder, func() error { return nil }, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "duplicates from_refs") {
		t.Fatalf("rewritten memory refs error = %v, want duplicate rejection", err)
	}
	if viewCalled {
		t.Fatal("memory-ref collision opened SnapshotView")
	}
	forwarder.mu.Lock()
	quiescing := forwarder.quiescing
	forwarder.mu.Unlock()
	if quiescing {
		t.Fatal("memory-ref collision reached the capture gate")
	}
}

func TestHandleSnapshotRequestRejectsPredictableErrorsBeforeQuiesce(t *testing.T) {
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(t.TempDir(), "must-not-dial.sock")},
	}
	baseCfg := &config.SandboxConfig{}
	baseOpts := RunOptions{
		Cfg: baseCfg, SandboxID: "test",
		PortableConfig: snapshotTestLivePortable(t),
	}
	disks := []SnapDiskRef{{DiffPath: filepath.Join(t.TempDir(), "not-needed.diff")}}

	_, err := handleSnapshotRequest(context.Background(), ctl.Request{}, baseOpts, nil, disks, nil,
		"", t.TempDir(), "", nil, nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "--output and --upload") {
		t.Fatalf("missing output error = %v", err)
	}

	_, err = handleSnapshotRequest(context.Background(), ctl.Request{Upload: true}, baseOpts, nil, disks, nil,
		"", t.TempDir(), "", nil, nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("missing manifest config error = %v", err)
	}

	validRequest := ctl.Request{OutDir: filepath.Join(t.TempDir(), "out")}
	_, err = handleSnapshotRequest(context.Background(), validRequest, RunOptions{
		PortableConfig: baseOpts.PortableConfig, SandboxID: "test",
	}, nil, disks, nil, "unused.sock", t.TempDir(), "", nil, nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "host configuration") {
		t.Fatalf("missing host config error = %v", err)
	}

	_, err = handleSnapshotRequest(context.Background(), validRequest, baseOpts, nil, disks, nil,
		"unused.sock", t.TempDir(), "", nil, nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "memory backing") {
		t.Fatalf("missing memory backing error = %v", err)
	}
}

func TestCaptureGateSerializesAndMakesDestroyCaptureTerminal(t *testing.T) {
	var gate captureGate
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		if err := gate.begin(); err != nil {
			t.Errorf("first capture admission: %v", err)
			close(done)
			return
		}
		close(entered)
		<-release
		gate.finish(false)
		close(done)
	}()
	<-entered
	if err := gate.begin(); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent capture admission error = %v", err)
	}
	close(release)
	<-done
	if err := gate.begin(); err != nil {
		t.Fatalf("capture after failed/resumed operation: %v", err)
	}
	gate.finish(true)
	if err := gate.begin(); err == nil || !strings.Contains(err.Error(), "shutdown is in progress") {
		t.Fatalf("capture after terminal commit error = %v", err)
	}
}

func TestSnapshotHandlerRejectsCaptureBeforeCloudHypervisorStarts(t *testing.T) {
	handler := &SnapshotHandler{CHProcess: func() processSignaler { return nil }}
	for _, capture := range []struct {
		name string
		call func() error
	}{
		{name: "export", call: func() error {
			_, err := handler.HandleExport(ctl.Request{ResumeAfter: true})
			return err
		}},
		{name: "snapshot", call: func() error {
			_, err := handler.Handle(ctl.Request{ResumeAfter: true})
			return err
		}},
	} {
		t.Run(capture.name, func(t *testing.T) {
			if err := capture.call(); err == nil || !strings.Contains(err.Error(), "process is not started") {
				t.Fatalf("capture before CH start error = %v", err)
			}
		})
	}
}

func TestHandleExportRequestResumeUsesExportFreezeWindow(t *testing.T) {
	dir := t.TempDir()
	guestSock := filepath.Join(dir, "guest.sock")
	events := &lifecycleEvents{}
	guestDone := serveOneQuiesce(t, guestSock, func(request *proto.Message) error {
		events.add("guest-quiesce")
		if !request.SkipDropCaches {
			return errors.New("export quiesce did not force skip_drop_caches")
		}
		return nil
	})

	resumeSeen := atomic.Bool{}
	chSock, chRequests := serveLifecycleCH(t, dir, func(path string) int {
		events.add("ch:" + path)
		if path == "/api/v1/vm.resume" {
			resumeSeen.Store(true)
		}
		if path == "/api/v1/vm.snapshot" {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})

	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	c0Before, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := NewForwarder("", discardLogf)
	viewCalls := 0
	reattachCalls := 0
	outputDir := filepath.Join(dir, "output")
	resp, err := handleExportRequest(
		context.Background(),
		ctl.Request{OutDir: outputDir, ResumeAfter: true},
		RunOptions{Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "test"},
		[]SnapDiskRef{{
			Size: 4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				viewCalls++
				forwarder.mu.Lock()
				gated := forwarder.quiescing
				forwarder.mu.Unlock()
				if !gated {
					return nil, nil, errors.New("SnapshotView opened before forward gate")
				}
				if forwarder.ExecAllowed() {
					return nil, nil, errors.New("SnapshotView opened before exec gate")
				}
				events.add("view")
				return bytes.NewReader(bytes.Repeat([]byte{0x51}, 4096)), nil, nil
			},
		}},
		nil,
		chSock,
		filepath.Join(dir, "run"),
		"",
		nil,
		nil,
		&guestlink.Pinger{Client: &guestlink.HostClient{BasePath: guestSock}},
		forwarder,
		func() error {
			reattachCalls++
			if !resumeSeen.Load() {
				return errors.New("reattach ran before CH resume")
			}
			events.add("reattach")
			return nil
		},
		nil,
		discardLogf,
	)
	if err != nil {
		t.Fatal(err)
	}
	if guestErr := waitLifecycleResult(t, guestDone); guestErr != nil {
		t.Fatal(guestErr)
	}
	requests := chRequests()
	if got, want := strings.Join(requests, ","), "/api/v1/vm.pause,/api/v1/vm.resume"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
	if viewCalls != 1 {
		t.Fatalf("SnapshotView calls = %d, want 1", viewCalls)
	}
	if reattachCalls != 1 {
		t.Fatalf("reattach calls = %d, want 1", reattachCalls)
	}
	if got, want := strings.Join(events.snapshot(), ","), "guest-quiesce,ch:/api/v1/vm.pause,view,ch:/api/v1/vm.resume,reattach"; got != want {
		t.Fatalf("export order = %q, want %q", got, want)
	}
	forwarder.mu.Lock()
	stillGated := forwarder.quiescing
	forwarder.mu.Unlock()
	if stillGated {
		t.Fatal("forwarder remained gated after export --resume")
	}
	if !forwarder.ExecAllowed() {
		t.Fatal("exec admission remained gated after export --resume")
	}
	c0After, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c0Before, c0After) {
		t.Fatal("live export mutated immutable C0")
	}
	if resp.SandboxPath == "" || resp.SandboxRef == "" {
		t.Fatalf("export response = %+v", resp)
	}
	alias, err := os.Readlink(filepath.Join(outputDir, "test.sandbox"))
	if err != nil {
		t.Fatalf("read committed Sandbox alias: %v", err)
	}
	if alias != filepath.Base(resp.SandboxPath) {
		t.Fatalf("Sandbox alias = %q, want %q", alias, filepath.Base(resp.SandboxPath))
	}
}

func TestHandleExportRequestResumeReattachFailureTerminatesVM(t *testing.T) {
	dir := t.TempDir()
	guestSock := filepath.Join(dir, "guest.sock")
	guestDone := serveOneQuiesce(t, guestSock, nil)
	chSock, chRequests := serveLifecycleCH(t, dir, func(path string) int {
		if path == "/api/v1/vmm.shutdown" {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	forwarder := NewForwarder("", discardLogf)
	outputDir := filepath.Join(dir, "output")
	chExited := make(chan struct{})
	process := &channelSignaler{sent: make(chan os.Signal, 1)}
	signalSeen := make(chan os.Signal, 1)
	go func() {
		signalSeen <- <-process.sent
		close(chExited)
	}()
	_, err := handleExportRequest(
		context.Background(),
		ctl.Request{OutDir: outputDir, ResumeAfter: true},
		RunOptions{Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "test"},
		[]SnapDiskRef{{
			Size: 4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				return bytes.NewReader(bytes.Repeat([]byte{0x52}, 4096)), nil, nil
			},
		}},
		nil, chSock, filepath.Join(dir, "run"), "", chExited, process,
		&guestlink.Pinger{Client: &guestlink.HostClient{BasePath: guestSock}},
		forwarder,
		func() error { return errors.New("injected resumed export reattach failure") },
		nil, discardLogf,
	)
	if err == nil || !strings.Contains(err.Error(), "injected resumed export reattach failure") {
		t.Fatalf("export resume error = %v", err)
	}
	if guestErr := waitLifecycleResult(t, guestDone); guestErr != nil {
		t.Fatal(guestErr)
	}
	if got, want := strings.Join(chRequests(), ","), "/api/v1/vm.pause,/api/v1/vm.resume,/api/v1/vmm.shutdown"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
	if signal := <-signalSeen; signal != syscall.SIGTERM {
		t.Fatalf("reattach failure fallback signal = %v, want SIGTERM", signal)
	}
	forwarder.mu.Lock()
	stillGated := forwarder.quiescing
	forwarder.mu.Unlock()
	if !stillGated {
		t.Fatal("forwarder reopened after terminal reattach failure")
	}
	if _, statErr := os.Lstat(filepath.Join(outputDir, "test.sandbox")); statErr != nil {
		t.Fatalf("committed export artifact was lost after recovery failure: %v", statErr)
	}
}

func TestHandleExportRequestFailureResumesBeforeReattachAndDoesNotCommitAlias(t *testing.T) {
	dir := t.TempDir()
	guestSock := filepath.Join(dir, "guest.sock")
	guestDone := serveOneQuiesce(t, guestSock, nil)
	var resumed atomic.Bool
	chSock, chRequests := serveLifecycleCH(t, dir, func(path string) int {
		if path == "/api/v1/vm.resume" {
			resumed.Store(true)
		}
		if path == "/api/v1/vm.snapshot" {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	forwarder := NewForwarder("", discardLogf)
	outputDir := filepath.Join(dir, "output")
	reattachCalls := 0
	_, err := handleExportRequest(
		context.Background(),
		ctl.Request{OutDir: outputDir},
		RunOptions{Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "test"},
		[]SnapDiskRef{{
			Size: 4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				return nil, nil, errors.New("injected disk capture failure")
			},
		}},
		nil,
		chSock,
		filepath.Join(dir, "run"),
		"",
		nil,
		nil,
		&guestlink.Pinger{Client: &guestlink.HostClient{BasePath: guestSock}},
		forwarder,
		func() error {
			reattachCalls++
			if !resumed.Load() {
				return errors.New("reattach ran before CH resume")
			}
			return errors.New("injected export reattach failure")
		},
		nil,
		discardLogf,
	)
	if err == nil || !strings.Contains(err.Error(), "injected disk capture failure") ||
		!strings.Contains(err.Error(), "injected export reattach failure") {
		t.Fatalf("export error = %v", err)
	}
	if guestErr := waitLifecycleResult(t, guestDone); guestErr != nil {
		t.Fatal(guestErr)
	}
	if got, want := strings.Join(chRequests(), ","), "/api/v1/vm.pause,/api/v1/vm.resume"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
	if !resumed.Load() || reattachCalls != 1 {
		t.Fatalf("failure recovery resumed=%v reattach_calls=%d", resumed.Load(), reattachCalls)
	}
	forwarder.mu.Lock()
	stillGated := forwarder.quiescing
	forwarder.mu.Unlock()
	if stillGated {
		t.Fatal("forwarder remained gated after failed export")
	}
	if _, statErr := os.Lstat(filepath.Join(outputDir, "test.sandbox")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed export committed root alias: %v", statErr)
	}
}

func TestHandleExportRequestRejectsPredictableErrorsBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	viewCalled := false
	disks := []SnapDiskRef{{Size: 4096, SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
		viewCalled = true
		return bytes.NewReader(make([]byte, 4096)), nil, nil
	}}}
	pinger := &guestlink.Pinger{Client: &guestlink.HostClient{BasePath: filepath.Join(dir, "must-not-dial.sock")}}
	opts := RunOptions{Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "test"}

	_, err := handleExportRequest(context.Background(), ctl.Request{}, opts, disks, nil,
		filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"), "", nil,
		nil, pinger, nil, nil, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "--output and --upload") {
		t.Fatalf("missing output error = %v", err)
	}
	if viewCalled {
		t.Fatal("predictable export error opened SnapshotView")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = handleExportRequest(ctx, ctl.Request{OutDir: filepath.Join(dir, "output")}, opts, disks, nil,
		filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"), "", nil,
		nil, pinger, nil, func() error { return nil }, nil, discardLogf)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export error = %v, want context.Canceled", err)
	}
	if viewCalled {
		t.Fatal("canceled export opened SnapshotView")
	}
}

func TestCaptureHandlersRejectProspectiveLayerLimitBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	portable := snapshotTestLivePortable(t)
	for i := 0; i < config.MaxPortableLayerRefs; i++ {
		portable.Boot.Root.BaseFromRefs = append(portable.Boot.Root.BaseFromRefs,
			"manifest://"+fmt.Sprintf("%064x", i+1))
	}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	parentRef := "manifest://" + strings.Repeat("f", 64)
	viewCalls := 0
	disks := []SnapDiskRef{{
		DiffPath: filepath.Join(dir, "active.diff"), Size: 4096,
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalls++
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		},
	}}
	baseOpts := RunOptions{
		Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "layer-limit",
		SourceBinding: &RunSourceBinding{SandboxRef: parentRef, RuntimeRef: parentRef},
	}
	forwarder := NewForwarder("", discardLogf)
	pinger := &guestlink.Pinger{Client: &guestlink.HostClient{BasePath: filepath.Join(dir, "must-not-dial.sock")}}

	_, err := handleExportRequest(context.Background(), ctl.Request{OutDir: filepath.Join(dir, "export")},
		baseOpts, disks, nil, filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"),
		"", nil, nil, pinger, forwarder, func() error { return nil }, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 entries") {
		t.Fatalf("export prospective C1 error = %v", err)
	}

	mfd, err := memory.Create("prospective-layer-limit", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	_, err = handleSnapshotRequest(context.Background(), ctl.Request{OutDir: filepath.Join(dir, "snapshot")},
		baseOpts, mfd, disks, nil, filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"),
		"", nil, nil, pinger, forwarder, func() error { return nil }, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "exceeds 64 entries") {
		t.Fatalf("snapshot prospective C1 error = %v", err)
	}
	if viewCalls != 0 {
		t.Fatalf("prospective C1 preflight opened SnapshotView %d times", viewCalls)
	}
	forwarder.mu.Lock()
	quiescing := forwarder.quiescing
	forwarder.mu.Unlock()
	if quiescing {
		t.Fatal("prospective C1 preflight gated forwards/exec")
	}
}

type lifecycleEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *lifecycleEvents) add(event string) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *lifecycleEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.events...)
}

func serveOneQuiesce(t *testing.T, path string, validate func(*proto.Message) error) <-chan error {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			done <- acceptErr
			return
		}
		defer conn.Close()
		line := make([]byte, len(proto.HostConnectLine))
		if _, readErr := io.ReadFull(conn, line); readErr != nil {
			done <- readErr
			return
		}
		if !bytes.Equal(line, proto.HostConnectLine) {
			done <- fmt.Errorf("CONNECT line = %q", line)
			return
		}
		if _, writeErr := conn.Write([]byte("OK 1\n")); writeErr != nil {
			done <- writeErr
			return
		}
		request, readErr := proto.ReadMessage(conn)
		if readErr != nil {
			done <- readErr
			return
		}
		if request.Type != proto.TypeQuiesce {
			done <- fmt.Errorf("guest request = %q, want %q", request.Type, proto.TypeQuiesce)
			return
		}
		if validate != nil {
			if validateErr := validate(request); validateErr != nil {
				done <- validateErr
				return
			}
		}
		done <- proto.WriteMessage(conn, &proto.Message{
			Type: proto.TypeQuiesced, DropCachesResult: proto.DropCachesSkipped,
		})
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return done
}

func serveLifecycleCH(t *testing.T, dir string, status func(string) int) (string, func() []string) {
	t.Helper()
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.URL.Path)
		mu.Unlock()
		code := http.StatusNoContent
		if status != nil {
			code = status(request.URL.Path)
		}
		w.WriteHeader(code)
	})}
	done := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(done)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	return sock, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), requests...)
	}
}

func waitLifecycleResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for lifecycle test peer")
		return nil
	}
}

func TestHandleSnapshotRequestQuiescesWithoutPinger(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	events := &lifecycleEvents{}
	guestDone := serveOneQuiesce(t, filepath.Join(runDir, "vsock.sock"), func(*proto.Message) error {
		events.add("guest-quiesce")
		return nil
	})
	chSock, chRequests := serveLifecycleCH(t, dir, func(path string) int {
		events.add("ch:" + path)
		if path == "/api/v1/vm.pause" {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	mfd, err := memory.Create("snapshot-no-pinger", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	viewCalled := false
	reattachCalls := 0
	_, err = handleSnapshotRequest(
		context.Background(), ctl.Request{OutDir: filepath.Join(dir, "out"), ResumeAfter: true},
		RunOptions{Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "test"},
		mfd, []SnapDiskRef{{
			DiffPath: filepath.Join(dir, "diff"), Size: 4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				viewCalled = true
				return bytes.NewReader(make([]byte, 4096)), nil, nil
			},
		}}, nil, chSock, runDir, "", nil, nil, nil, nil,
		func() error {
			reattachCalls++
			events.add("reattach")
			return nil
		}, nil, discardLogf,
	)
	if err == nil || !strings.Contains(err.Error(), "CH pause") {
		t.Fatalf("snapshot error = %v, want CH pause failure", err)
	}
	if guestErr := waitLifecycleResult(t, guestDone); guestErr != nil {
		t.Fatal(guestErr)
	}
	if got, want := strings.Join(chRequests(), ","), "/api/v1/vm.pause"; got != want {
		t.Fatalf("CH requests = %q, want %q", got, want)
	}
	if got, want := strings.Join(events.snapshot(), ","), "guest-quiesce,ch:/api/v1/vm.pause,reattach"; got != want {
		t.Fatalf("snapshot recovery order = %q, want %q", got, want)
	}
	if viewCalled {
		t.Fatal("disk SnapshotView opened after failed CH pause")
	}
	if reattachCalls != 1 {
		t.Fatalf("reattach calls = %d, want 1", reattachCalls)
	}
}

func TestHandleSnapshotRequestValidatesDiskMergeBaseBeforeQuiesce(t *testing.T) {
	dir := t.TempDir()
	diff := filepath.Join(dir, "must-not-be-opened.diff")
	digest := strings.Repeat("0", 64)
	cfg := &config.SandboxConfig{}
	parentRef := "file://base.overlay@sha256:" + digest
	pinger := &guestlink.Pinger{
		Client: &guestlink.HostClient{BasePath: filepath.Join(dir, "must-not-dial.sock")},
	}
	mergeRef := false
	req := ctl.Request{OutDir: filepath.Join(dir, "out"), MergeRef: &mergeRef}
	viewCalled := false
	mfd, err := memory.Create("disk-merge-preflight", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()

	_, err = handleSnapshotRequest(context.Background(), req, RunOptions{
		Cfg: cfg, SandboxID: "test",
		PortableConfig: snapshotTestLivePortable(t),
		SourceBinding: &RunSourceBinding{
			SandboxRef: parentRef, RuntimeRef: parentRef, RelativeDir: dir,
		},
	}, mfd,
		[]SnapDiskRef{{
			DiffPath: diff,
			Size:     4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				viewCalled = true
				return bytes.NewReader(make([]byte, 4096)), nil, nil
			},
		}}, nil, filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"),
		"", nil, nil, pinger, nil, func() error { return nil }, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "disk 0 merge base") {
		t.Fatalf("disk merge preflight error = %v", err)
	}
	if viewCalled {
		t.Fatal("snapshot view provider called during size preflight")
	}
}

func TestHandleSnapshotRequestResolvesUploadKeyBeforeSnapshotView(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.SandboxConfig{}
	viewCalled := false
	keyCalls := 0
	mfd, err := memory.Create("upload-key-preflight", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	_, err = handleSnapshotRequest(context.Background(), ctl.Request{Upload: true}, RunOptions{
		Cfg:            cfg,
		PortableConfig: snapshotTestLivePortable(t),
		SandboxID:      "test",
		ManifestCfg:    &config.ManifestConfig{Store: manifest.StoreConfig{Endpoint: "unused"}},
		CustomerKeyFn: func() ([32]byte, error) {
			keyCalls++
			return [32]byte{}, errors.New("invalid customer key")
		},
	}, mfd, []SnapDiskRef{{
		DiffPath: filepath.Join(dir, "diff"),
		Size:     4096,
		SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
			viewCalled = true
			return bytes.NewReader(make([]byte, 4096)), nil, nil
		},
	}}, nil, filepath.Join(dir, "must-not-call-ch.sock"), filepath.Join(dir, "run"), "", nil, nil, nil, nil, func() error { return nil }, nil, discardLogf)
	if err == nil || !strings.Contains(err.Error(), "customer key") {
		t.Fatalf("upload key error = %v", err)
	}
	if keyCalls != 1 {
		t.Fatalf("customer key calls=%d, want 1", keyCalls)
	}
	if viewCalled {
		t.Fatal("snapshot view was opened before customer-key validation")
	}
}

func TestHandleSnapshotRequestTerminatesAfterFailedRecoveryReattach(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vsock.sock")
	listener, err := net.Listen("unix", base)
	if err != nil {
		t.Fatal(err)
	}
	memoryHighPath := filepath.Join(dir, "memory.high")
	if err := os.WriteFile(memoryHighPath, []byte("234881024\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var quiesceSawLiftedMemoryHigh atomic.Bool
	guestDone := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			guestDone <- acceptErr
			return
		}
		defer conn.Close()
		line := make([]byte, len(proto.HostConnectLine))
		if _, readErr := io.ReadFull(conn, line); readErr != nil {
			guestDone <- readErr
			return
		}
		if string(line) != string(proto.HostConnectLine) {
			guestDone <- fmt.Errorf("CONNECT line = %q", line)
			return
		}
		if _, writeErr := conn.Write([]byte("OK 1\n")); writeErr != nil {
			guestDone <- writeErr
			return
		}
		request, readErr := proto.ReadMessage(conn)
		if readErr != nil {
			guestDone <- readErr
			return
		}
		if request.Type != proto.TypeQuiesce {
			guestDone <- fmt.Errorf("request type = %q", request.Type)
			return
		}
		value, readErr := os.ReadFile(memoryHighPath)
		quiesceSawLiftedMemoryHigh.Store(readErr == nil && strings.TrimSpace(string(value)) == "max")
		guestDone <- proto.WriteMessage(conn, &proto.Message{
			Type:             proto.TypeQuiesced,
			DropCachesResult: proto.DropCachesSkipped,
		})
	}()
	t.Cleanup(func() { _ = listener.Close() })
	chSock := filepath.Join(dir, "ch.sock")
	chListener, err := net.Listen("unix", chSock)
	if err != nil {
		t.Fatal(err)
	}
	var resumed atomic.Bool
	var recoveryShutdown atomic.Bool
	var pauseSawLiftedMemoryHigh atomic.Bool
	chServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/vm.pause" {
			value, readErr := os.ReadFile(memoryHighPath)
			pauseSawLiftedMemoryHigh.Store(readErr == nil && strings.TrimSpace(string(value)) == "max")
		}
		if r.URL.Path == "/api/v1/vm.snapshot" {
			http.Error(w, "injected snapshot failure", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/api/v1/vm.resume" {
			resumed.Store(true)
		}
		if r.URL.Path == "/api/v1/vmm.shutdown" {
			recoveryShutdown.Store(true)
			http.Error(w, "injected recovery shutdown failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	chDone := make(chan struct{})
	go func() {
		_ = chServer.Serve(chListener)
		close(chDone)
	}()
	t.Cleanup(func() {
		_ = chServer.Close()
		<-chDone
	})

	mfd, err := memory.Create("snapshot-failure-reattach", 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer mfd.Close()
	reattachCalls := 0
	reattachedAfterResume := false
	chExited := make(chan struct{})
	process := &channelSignaler{sent: make(chan os.Signal, 1)}
	signalSeen := make(chan os.Signal, 1)
	go func() {
		signalSeen <- <-process.sent
		close(chExited)
	}()
	portable := snapshotTestPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := portable.Validate(); err != nil {
		t.Fatal(err)
	}
	_, err = handleSnapshotRequest(
		context.Background(),
		ctl.Request{OutDir: filepath.Join(dir, "out")},
		RunOptions{Cfg: &config.SandboxConfig{}, PortableConfig: portable, SandboxID: "test"},
		mfd,
		[]SnapDiskRef{{
			DiffPath: filepath.Join(dir, "diff"),
			Size:     4096,
			SnapshotView: func() (io.ReadSeeker, []sparse.Extent, error) {
				return bytes.NewReader(make([]byte, 4096)), nil, nil
			},
		}},
		nil,
		chSock,
		filepath.Join(dir, "run"),
		dir,
		chExited,
		process,
		&guestlink.Pinger{Client: &guestlink.HostClient{BasePath: base}},
		nil,
		func() error {
			reattachCalls++
			if !resumed.Load() {
				return errors.New("reattach ran before VM resume")
			}
			reattachedAfterResume = true
			return errors.New("injected snapshot reattach failure")
		},
		nil,
		discardLogf,
	)
	if err == nil || !strings.Contains(err.Error(), "CH snapshot") ||
		!strings.Contains(err.Error(), "injected snapshot reattach failure") {
		t.Fatalf("snapshot error = %v, want CH snapshot and reattach failures", err)
	}
	if guestErr := <-guestDone; guestErr != nil {
		t.Fatal(guestErr)
	}
	if reattachCalls != 1 {
		t.Fatalf("reattach calls = %d, want 1", reattachCalls)
	}
	if !resumed.Load() {
		t.Fatal("VM was not resumed before failed snapshot returned")
	}
	if !reattachedAfterResume {
		t.Fatal("guest was not reattached after VM resume")
	}
	if !recoveryShutdown.Load() {
		t.Fatal("failed snapshot recovery did not request VMM shutdown")
	}
	if signal := <-signalSeen; signal != syscall.SIGTERM {
		t.Fatalf("snapshot recovery fallback signal = %v, want SIGTERM", signal)
	}
	if !quiesceSawLiftedMemoryHigh.Load() {
		t.Fatal("guest quiesce did not run with memory.high lifted")
	}
	if !pauseSawLiftedMemoryHigh.Load() {
		t.Fatal("VM pause did not run with memory.high lifted")
	}
	if value, readErr := os.ReadFile(memoryHighPath); readErr != nil {
		t.Fatal(readErr)
	} else if string(value) != "234881024\n" {
		t.Fatalf("memory.high = %q after failed snapshot, want original value", value)
	}
}

func snapshotTestPortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	key := strings.Repeat("a", 64)
	cfg := &config.PortableSandboxConfig{
		Version: 1,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel: "file://kernel@sha256:" + key, Runtime: "file://runtime@sha256:" + key,
			Root: config.PortableRootConfig{Base: "file://base@sha256:" + key, Overlay: &config.PortableOverlayConfig{Base: "self"}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/true", Workdir: "/", Restart: "never"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func snapshotTestLivePortable(t *testing.T) *config.PortableSandboxConfig {
	t.Helper()
	cfg := snapshotTestPortable(t)
	cfg.Boot.Root = config.PortableRootConfig{Base: "self"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMergeBaseOpenersExcludeSandboxAndSnapshotZIPTails(t *testing.T) {
	testRef := func(raw, path string) string {
		t.Helper()
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			t.Fatal(err)
		}
		ref.Path = path
		return ref.String()
	}
	directory := t.TempDir()
	sink := snapshot.NewFileSink(directory, "merge-openers", nil, false, nil)

	portableBytes, err := config.MarshalPortableSandboxConfig(snapshotTestPortable(t))
	if err != nil {
		t.Fatal(err)
	}
	diskBytes := bytes.Repeat([]byte{0x61}, 8192)
	diskSource, err := sandboxfile.BuildSource(sparse.Dense(bytes.NewReader(diskBytes), uint64(len(diskBytes))), nil, portableBytes)
	if err != nil {
		t.Fatal(err)
	}
	diskRef, diskPath, err := sink.AbsorbSandbox(context.Background(), diskSource)
	if err != nil {
		t.Fatal(err)
	}
	diskStream, err := newDiskMergeBaseOpener(RunOptions{})(context.Background(), testRef(diskRef, diskPath))
	if err != nil {
		t.Fatal(err)
	}
	defer diskStream.Close()
	if diskStream.Size() != uint64(len(diskBytes)) {
		t.Fatalf("disk merge source size=%d, want payload %d", diskStream.Size(), len(diskBytes))
	}

	snapshotCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version:    snapshot.SnapshotConfigVersion,
		SandboxRef: "manifest://" + strings.Repeat("b", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	memoryBytes := bytes.Repeat([]byte{0x71}, 4096)
	memorySource, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(memoryBytes), uint64(len(memoryBytes))),
		[]byte("{}"), []byte("{}"), snapshotCfg,
	)
	if err != nil {
		t.Fatal(err)
	}
	memoryRef, memoryPath, err := sink.AbsorbSnapshot(context.Background(), memorySource)
	if err != nil {
		t.Fatal(err)
	}
	memoryStream, err := newMemoryMergeBaseOpener(RunOptions{})(context.Background(), testRef(memoryRef, memoryPath))
	if err != nil {
		t.Fatal(err)
	}
	defer memoryStream.Close()
	if memoryStream.Size() != uint64(len(memoryBytes)) {
		t.Fatalf("memory merge source size=%d, want payload %d", memoryStream.Size(), len(memoryBytes))
	}
}

// fakeSignaler records signals sent to it; never blocks. Goroutine-safe:
// waitForCHWithSignalEscalation calls Signal from the test goroutine while
// watcher goroutines poll the recorded list.
type fakeSignaler struct {
	mu   sync.Mutex
	sent []os.Signal
}

type channelSignaler struct{ sent chan os.Signal }

func (s *channelSignaler) Signal(sig os.Signal) error {
	s.sent <- sig
	return nil
}

func (f *fakeSignaler) Signal(sig os.Signal) error {
	f.mu.Lock()
	f.sent = append(f.sent, sig)
	f.mu.Unlock()
	return nil
}

func (f *fakeSignaler) sentCount(sig os.Signal) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sent {
		if s == sig {
			n++
		}
	}
	return n
}

func discardLogf(string, ...any) {}

func TestVMMMemoryHighLifecycleGuard(t *testing.T) {
	t.Run("restore original value", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "memory.high")
		if err := os.WriteFile(path, []byte("234881024\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if memoryHighLock != nil {
				_ = memoryHighLock.Close()
			}
		}()
		if value, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if string(value) != "max" {
			t.Fatalf("lifted memory.high = %q, want max", value)
		}
		restored, err := restoreVMMMemoryHigh(dir, previous)
		if err != nil {
			t.Fatal(err)
		}
		if !restored {
			t.Fatal("original memory.high was not restored")
		}
		if err := memoryHighLock.Close(); err != nil {
			t.Fatal(err)
		}
		memoryHighLock = nil
		if value, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if string(value) != "234881024\n" {
			t.Fatalf("restored memory.high = %q, want original value", value)
		}
	})

	t.Run("serialize local Budget high update", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "memory.high")
		if err := os.WriteFile(path, []byte("234881024"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A local high update must read the host VMM charge as a safety lower
		// bound. Model the cgroup input explicitly; an absent memory.current is
		// intentionally fail-closed and would keep the controller retrying.
		if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("0"), 0o644); err != nil {
			t.Fatal(err)
		}
		previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if memoryHighLock != nil {
				_ = memoryHighLock.Close()
			}
		}()
		cfg := &config.SandboxConfig{Resources: config.ResourcesConfig{
			Capacity:    config.CapacityConfig{Memory: "8GiB"},
			Allocatable: config.AllocatableConfig{Memory: "8GiB"},
		}}
		cfg.ApplyDefaults()
		memoryCtl, err := resctl.NewMemoryController(resctl.MemoryControllerOptions{
			Config: cfg, CgroupPath: dir, InitialBudget: 8 << 30, Logf: discardLogf,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		memoryCtl.StartCold(ctx)
		defer memoryCtl.Stop()
		if !memoryCtl.SubmitGuestReport(proto.MemReport{Epoch: 1, Seq: 1, MemTotalBytes: 8 << 30, MemAvailableBytes: 8 << 30}) {
			t.Fatal("report rejected")
		}
		time.Sleep(25 * time.Millisecond)
		if value, err := os.ReadFile(path); err != nil {
			t.Fatal(err)
		} else if string(value) != "max" {
			t.Fatalf("memory.high changed during lifecycle operation: %q", value)
		}

		restored, err := restoreVMMMemoryHigh(dir, previous)
		if err != nil {
			t.Fatal(err)
		}
		if !restored {
			t.Fatal("original memory.high was not restored before releasing lifecycle lock")
		}
		if err := memoryHighLock.Close(); err != nil {
			t.Fatal(err)
		}
		memoryHighLock = nil
		deadline := time.Now().Add(time.Second)
		for {
			value, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(value) != "234881024" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("local Budget update remained blocked after lifecycle lock release")
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func TestDestroyAfterSnapshotFallsBackToSIGTERMAndRetainsBarrier(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/vmm.shutdown" {
			t.Errorf("shutdown request = %s %s", r.Method, r.URL.Path)
		}
		requestSeen <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	chExited := make(chan struct{})
	barrierReleased := make(chan struct{})
	process := &channelSignaler{sent: make(chan os.Signal, 2)}
	go destroyAfterSnapshot(sock, process, nil, func() { close(barrierReleased) }, chExited, time.Second, discardLogf)

	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("destroy shutdown request was not sent")
	}
	select {
	case signal := <-process.sent:
		if signal != syscall.SIGTERM {
			t.Fatalf("shutdown fallback signal = %v, want SIGTERM", signal)
		}
	case <-time.After(time.Second):
		t.Fatal("failed vmm.shutdown did not signal the CH process")
	}
	select {
	case <-barrierReleased:
		t.Fatal("destroy barrier released after failed shutdown while VMM was still live")
	case <-time.After(25 * time.Millisecond):
	}
	close(chExited)
	select {
	case <-barrierReleased:
	case <-time.After(time.Second):
		t.Fatal("destroy barrier was not released after VMM exit")
	}
}

func TestDestroyAfterSnapshotReleasesBarrierWhenExitNotificationNeverArrives(t *testing.T) {
	chSock := filepath.Join(t.TempDir(), "missing-ch.sock")
	chExited := make(chan struct{})
	barrierReleased := make(chan struct{})
	process := &channelSignaler{sent: make(chan os.Signal, 2)}
	done := make(chan struct{})
	go func() {
		destroyAfterSnapshotWithBounds(
			chSock,
			process,
			nil,
			func() { close(barrierReleased) },
			chExited,
			time.Second,
			0,
			0,
			discardLogf,
		)
		close(done)
	}()

	for _, want := range []os.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		select {
		case got := <-process.sent:
			if got != want {
				t.Fatalf("destroy signal = %v, want %v", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("destroy did not send %v", want)
		}
	}
	select {
	case <-barrierReleased:
	case <-time.After(time.Second):
		t.Fatal("destroy did not release lifecycle barrier after bounded SIGKILL wait")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("destroy goroutine remained blocked without an exit notification")
	}
}

// TestWaitForCH_NoSignals: clean-exit path — doneCh fires before any
// signal, helper returns immediately with the wait error.
func TestWaitForCH_NoSignals(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	doneCh <- nil
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, time.Second, discardLogf)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(proc.sent) != 0 {
		t.Fatalf("expected no signals sent, got %v", proc.sent)
	}
}

// TestWaitForCH_SIGTERM_GracefulExit: SIGTERM arrives, CH exits within
// grace — only SIGTERM forwarded, no SIGKILL.
func TestWaitForCH_SIGTERM_GracefulExit(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}

	go func() {
		sigCh <- syscall.SIGTERM
		time.Sleep(50 * time.Millisecond)
		doneCh <- &exitErrStub{code: 0}
	}()

	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, 5*time.Second, discardLogf)
	if err == nil {
		t.Fatal("expected non-nil exit err stub")
	}
	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 SIGTERM, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGKILL) != 0 {
		t.Fatalf("expected no SIGKILL, got %v", proc.sent)
	}
}

func TestWaitForCH_OrderedShutdownLiftsMemoryHigh(t *testing.T) {
	dir := t.TempDir()
	memoryHighPath := filepath.Join(dir, "memory.high")
	if err := os.WriteFile(memoryHighPath, []byte("234881024"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.current"), []byte("123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan bool, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, readErr := os.ReadFile(memoryHighPath)
		requestSeen <- r.Method == http.MethodPut && r.URL.Path == "/api/v1/vmm.shutdown" &&
			readErr == nil && strings.TrimSpace(string(value)) == "max"
		w.WriteHeader(http.StatusNoContent)
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	var liftedAtRequest atomic.Bool
	go func() {
		select {
		case liftedAtRequestValue := <-requestSeen:
			liftedAtRequest.Store(liftedAtRequestValue)
		case <-time.After(time.Second):
		}
		doneCh <- nil
	}()
	sigCh <- syscall.SIGTERM
	if err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, sock, dir, time.Second, time.Second, discardLogf); err != nil {
		t.Fatal(err)
	}
	if !liftedAtRequest.Load() {
		t.Fatal("ordered shutdown request did not observe memory.high=max")
	}
	if len(proc.sent) != 0 {
		t.Fatalf("ordered shutdown unexpectedly used process signals: %v", proc.sent)
	}
	if value, err := os.ReadFile(memoryHighPath); err != nil {
		t.Fatal(err)
	} else if string(value) != "max" {
		t.Fatalf("memory.high = %q after ordered shutdown request, want max until VMM exit", value)
	}
}

func TestVMMMemoryHighThrottleDrainDelay(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "memory.current")
	eventsPath := filepath.Join(dir, "memory.events.local")
	if err := os.WriteFile(eventsPath, []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(currentPath, []byte("280408064\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("234881024\n")); got != memoryHighThrottleDrain {
		t.Fatalf("over-high drain delay = %s, want %s", got, memoryHighThrottleDrain)
	}
	if err := os.WriteFile(currentPath, []byte("200000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("234881024\n")); got != memoryHighThrottleDrain {
		t.Fatalf("prior-high-event drain delay = %s, want %s", got, memoryHighThrottleDrain)
	}
	if err := os.WriteFile(eventsPath, []byte("low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("234881024\n")); got != 0 {
		t.Fatalf("never-throttled drain delay = %s, want 0", got)
	}
	if got := vmmMemoryHighThrottleDrainDelay(dir, []byte("max\n")); got != 0 {
		t.Fatalf("unlimited-high drain delay = %s, want 0", got)
	}
}

func TestWaitForCH_BriefMemoryHighLockContentionRetriesOrderedShutdown(t *testing.T) {
	dir := t.TempDir()
	memoryHighPath := filepath.Join(dir, "memory.high")
	for name, value := range map[string]string{
		"memory.high":         "234881024",
		"memory.current":      "123456789",
		"memory.events.local": "low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	memoryHighLock, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer memoryHighLock.Close()
	if err := unix.Flock(int(memoryHighLock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			_ = unix.Flock(int(memoryHighLock.Fd()), unix.LOCK_UN)
		}
	}()

	sock := filepath.Join(dir, "ch.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	requestSeen := make(chan bool, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, readErr := os.ReadFile(memoryHighPath)
		requestSeen <- r.Method == http.MethodPut && r.URL.Path == "/api/v1/vmm.shutdown" &&
			readErr == nil && strings.TrimSpace(string(value)) == "max"
		w.WriteHeader(http.StatusNoContent)
	})}
	serverDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serverDone)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
	})

	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	retryStarted := make(chan struct{}, 1)
	logf := func(format string, _ ...any) {
		if strings.Contains(format, "retrying for up to %s before SIGTERM fallback") {
			select {
			case retryStarted <- struct{}{}:
			default:
			}
		}
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			sock,
			dir,
			time.Second,
			time.Second,
			logf,
		)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not retry the memory.high lifecycle lock")
	}
	if proc.sentCount(syscall.SIGTERM) != 0 {
		t.Fatalf("brief memory.high lifecycle contention caused fallback signal: %v", proc.sent)
	}
	if err := unix.Flock(int(memoryHighLock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	lockHeld = false
	select {
	case lifted := <-requestSeen:
		if !lifted {
			t.Fatal("retried ordered shutdown did not observe memory.high=max")
		}
	case <-time.After(time.Second):
		t.Fatal("ordered shutdown was not attempted after memory.high lifecycle lock release")
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not return after VMM exit")
	}
	if len(proc.sent) != 0 {
		t.Fatalf("retried ordered shutdown unexpectedly used process signals: %v", proc.sent)
	}
}

func TestWaitForCH_PersistentLifecycleLockBusyFallsBackToSIGTERM(t *testing.T) {
	dir := t.TempDir()
	memoryHighPath := filepath.Join(dir, "memory.high")
	if err := os.WriteFile(memoryHighPath, []byte("234881024"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
	if err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan error, 1)
	defer func() {
		select {
		case doneCh <- nil:
		default:
		}
		_, _ = restoreVMMMemoryHigh(dir, previous)
		_ = memoryHighLock.Close()
	}()

	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			filepath.Join(dir, "unused.sock"),
			dir,
			time.Second,
			time.Second,
			discardLogf,
		)
	}()

	start := time.Now()
	sigCh <- syscall.SIGTERM
	for proc.sentCount(syscall.SIGTERM) == 0 && time.Since(start) <= 500*time.Millisecond {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown signal blocked on lifecycle lock for %s", elapsed)
	} else if elapsed < memoryHighLockRetryWindow {
		t.Fatalf("shutdown fell back before the controller retry window elapsed: %s", elapsed)
	}
	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 fallback SIGTERM, got %v", proc.sent)
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not return after VMM exit")
	}
}

func TestWaitForCH_LockRetryRemainsSignalResponsive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.high"), []byte("234881024"), 0o644); err != nil {
		t.Fatal(err)
	}
	previous, memoryHighLock, err := liftVMMMemoryHigh(dir)
	if err != nil {
		t.Fatal(err)
	}
	doneCh := make(chan error, 1)
	defer func() {
		select {
		case doneCh <- nil:
		default:
		}
		_, _ = restoreVMMMemoryHigh(dir, previous)
		_ = memoryHighLock.Close()
	}()

	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	retryStarted := make(chan struct{}, 1)
	logf := func(format string, _ ...any) {
		if strings.Contains(format, "retrying for up to %s before SIGTERM fallback") {
			select {
			case retryStarted <- struct{}{}:
			default:
			}
		}
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			filepath.Join(dir, "unused.sock"),
			dir,
			time.Second,
			time.Second,
			logf,
		)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not enter the memory.high lifecycle lock retry window")
	}
	start := time.Now()
	sigCh <- syscall.SIGINT
	for proc.sentCount(syscall.SIGKILL) == 0 && time.Since(start) <= 500*time.Millisecond {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("second signal was blocked by memory.high lifecycle lock retry for %s", elapsed)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected immediate SIGKILL during memory.high lifecycle lock retry, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGTERM) != 0 {
		t.Fatalf("memory.high lifecycle lock retry unexpectedly fell back before escalation: %v", proc.sent)
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not reap VMM after retry escalation")
	}
}

func TestWaitForCH_ThrottleDrainRemainsSignalResponsive(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{
		"memory.high":         "234881024",
		"memory.current":      "280408064",
		"memory.events.local": "low 0\nhigh 1\nmax 0\noom 0\noom_kill 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	drainStarted := make(chan struct{}, 1)
	logf := func(format string, _ ...any) {
		if strings.Contains(format, "waiting %s for existing VMM memory.high throttles") {
			select {
			case drainStarted <- struct{}{}:
			default:
			}
		}
	}
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForCHWithSignalEscalation(
			doneCh,
			sigCh,
			proc,
			1234,
			filepath.Join(dir, "unused.sock"),
			dir,
			time.Second,
			10*time.Second,
			logf,
		)
	}()

	sigCh <- syscall.SIGTERM
	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not enter memory.high drain")
	}
	start := time.Now()
	sigCh <- syscall.SIGINT
	for proc.sentCount(syscall.SIGKILL) == 0 && time.Since(start) <= 500*time.Millisecond {
		time.Sleep(time.Millisecond)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("second signal was blocked by throttle drain for %s", elapsed)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected immediate SIGKILL during throttle drain, got %v", proc.sent)
	}
	doneCh <- nil
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait helper did not reap VMM after drain escalation")
	}
}

// TestWaitForCH_SIGTERM_EscalatesToSIGKILL: CH ignores SIGTERM, helper
// must escalate to SIGKILL after grace expires. This is the regression
// test for the >50min hang observed in cold-target.
func TestWaitForCH_SIGTERM_EscalatesToSIGKILL(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 1)
	proc := &fakeSignaler{}
	grace := 100 * time.Millisecond

	// Simulate stuck CH: doneCh never fires until we manually signal it.
	// SIGKILL handling: once helper sends SIGKILL we treat as "process
	// died" and unblock doneCh.
	var killSeen atomic.Bool
	go func() {
		for {
			if killSeen.Load() {
				doneCh <- &exitErrStub{code: -1}
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	// Watch for SIGKILL appearance (via the goroutine-safe accessor).
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			if proc.sentCount(syscall.SIGKILL) > 0 {
				killSeen.Store(true)
				return
			}
		}
	}()

	t0 := time.Now()
	sigCh <- syscall.SIGTERM
	err := waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, grace, discardLogf)
	elapsed := time.Since(t0)
	if err == nil {
		t.Fatal("expected exit err stub")
	}

	if proc.sentCount(syscall.SIGTERM) != 1 {
		t.Fatalf("expected 1 SIGTERM, got %v", proc.sent)
	}
	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected 1 SIGKILL escalation, got sent=%v", proc.sent)
	}
	// Must have waited at least the grace period before SIGKILL.
	if elapsed < grace {
		t.Fatalf("escalation fired too early: %v < %v", elapsed, grace)
	}
	// And not waited far longer (would indicate hang).
	if elapsed > grace+500*time.Millisecond {
		t.Fatalf("escalation took too long: %v (grace=%v)", elapsed, grace)
	}
}

// TestWaitForCH_DoubleSIGTERM_EscalatesImmediately: second SIGTERM
// during shutdown grace must skip the timer and SIGKILL right away.
func TestWaitForCH_DoubleSIGTERM_EscalatesImmediately(t *testing.T) {
	doneCh := make(chan error, 1)
	sigCh := make(chan os.Signal, 2)
	proc := &fakeSignaler{}
	grace := 10 * time.Second // long — would mask immediate escalation if buggy

	var killSeen atomic.Bool
	go func() {
		for !killSeen.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		doneCh <- &exitErrStub{code: -1}
	}()
	go func() {
		for {
			time.Sleep(2 * time.Millisecond)
			if proc.sentCount(syscall.SIGKILL) > 0 {
				killSeen.Store(true)
				return
			}
		}
	}()

	t0 := time.Now()
	sigCh <- syscall.SIGTERM
	time.Sleep(50 * time.Millisecond) // first SIGTERM arms timer
	sigCh <- syscall.SIGINT           // second signal escalates
	_ = waitForCHWithSignalEscalation(doneCh, sigCh, proc, 1234, "", "", 0, grace, discardLogf)
	elapsed := time.Since(t0)

	if proc.sentCount(syscall.SIGKILL) != 1 {
		t.Fatalf("expected 1 SIGKILL via immediate-escalate path, got sent=%v", proc.sent)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("immediate escalation took too long: %v", elapsed)
	}
}

func TestValidateSandboxIDRejectsPathComponents(t *testing.T) {
	for _, sandboxID := range []string{"", ".", "..", "../escape", "nested/id", `nested\id`} {
		if err := validateSandboxID(sandboxID); err == nil {
			t.Fatalf("validateSandboxID(%q) succeeded", sandboxID)
		}
	}
	if err := validateSandboxID("sandbox-123"); err != nil {
		t.Fatalf("validateSandboxID(valid): %v", err)
	}
}

// exitErrStub matches the *exec.ExitError shape just enough for callers
// that check via type assertion; ours doesn't, but we use it to be
// explicit about "process exited unsuccessfully".
type exitErrStub struct{ code int }

func (e *exitErrStub) Error() string { return fmt.Sprintf("exit %d", e.code) }

func init() {
	// Silence "imported and not used" if the file is included in builds
	// where errors is not referenced elsewhere.
	_ = errors.New
}
