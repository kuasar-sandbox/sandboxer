package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"gopkg.in/yaml.v3"
)

func TestPublishLocalToLocationRewritesLocalRefsAndRejectsInconsistentFinal(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	targetDir := t.TempDir()

	overlayPath, overlayDigest := writePublishArtifact(t, sourceDir, ".overlay", bytes.Repeat([]byte{0xA5}, 4096))
	basePath, baseDigest := writePublishArtifact(t, sourceDir, ".erofs", bytes.Repeat([]byte{0x5A}, 4096))
	manifestKey := strings.Repeat("b", 64)
	oldLocated := "file://old.overlay@location:old"
	snapCfg := &SnapshotCfg{FromRefs: []string{"manifest://" + manifestKey}}
	snapCfg.Resources.Capacity.CPU = 1
	snapCfg.Resources.Capacity.Memory = "4KiB"
	snapCfg.Boot.RuntimeRef = "file://runtime.bundle@sha256:" + strings.Repeat("c", 64)
	snapCfg.Boot.Root.BaseRef = "file://" + filepath.Base(basePath) + "@sha256:" + strings.TrimPrefix(baseDigest, "sha256:")
	snapCfg.Boot.Root.Overlay = &SnapOverlayCfg{
		Base:         "file://" + filepath.Base(overlayPath),
		BaseFromRefs: []string{oldLocated},
	}
	rootPath := writePublishSnapshot(t, sourceDir, snapCfg)
	if err := os.Chmod(sourceDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceDir, 0o755) })

	rootRef, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsedRoot, err := manifest.ParseRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	if !parsedRoot.Portable() || parsedRoot.Location != "shared" || filepath.Ext(parsedRoot.Path) != ".snapshot" {
		t.Fatalf("root ref = %#v", parsedRoot)
	}
	publishedOverlay := filepath.Join(targetDir, strings.TrimPrefix(overlayDigest, "sha256:")+".overlay")
	if _, err := os.Stat(publishedOverlay); err != nil {
		t.Fatalf("published overlay: %v", err)
	}
	publishedBase := filepath.Join(targetDir, strings.TrimPrefix(baseDigest, "sha256:")+".erofs")
	if _, err := os.Stat(publishedBase); err != nil {
		t.Fatalf("published EROFS base: %v", err)
	}
	publishedRoot := filepath.Join(targetDir, parsedRoot.Path)
	for _, path := range []string{publishedRoot, publishedOverlay, publishedBase} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Fatalf("published mode for %s = %o, want 644", filepath.Base(path), got)
		}
	}

	rootStream, _, _, err := openTarArtifact(publishedRoot, manifest.Ref{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_, publishedCfg, err := readSnapshotEntries(ctx, rootStream, int64(rootStream.Size()))
	rootStream.Close()
	if err != nil {
		t.Fatal(err)
	}
	wantOverlayRef := "file://" + filepath.Base(publishedOverlay) + "@sha256:" + strings.TrimPrefix(overlayDigest, "sha256:") + "@location:shared"
	if publishedCfg.Boot.Root.Overlay.Base != wantOverlayRef {
		t.Fatalf("overlay ref = %q, want %q", publishedCfg.Boot.Root.Overlay.Base, wantOverlayRef)
	}
	wantBaseRef := "file://" + filepath.Base(publishedBase) + "@sha256:" + strings.TrimPrefix(baseDigest, "sha256:") + "@location:shared"
	if publishedCfg.Boot.Root.BaseRef != wantBaseRef {
		t.Fatalf("base ref = %q, want %q", publishedCfg.Boot.Root.BaseRef, wantBaseRef)
	}
	if publishedCfg.Boot.Root.Overlay.BaseFromRefs[0] != oldLocated {
		t.Fatalf("existing located ref changed: %v", publishedCfg.Boot.Root.Overlay.BaseFromRefs)
	}
	if publishedCfg.FromRefs[0] != "manifest://"+manifestKey {
		t.Fatalf("manifest ref changed: %v", publishedCfg.FromRefs)
	}
	reusedRef, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil, false, nil)
	if err != nil {
		t.Fatalf("reuse consistent finals: %v", err)
	}
	if reusedRef != rootRef {
		t.Fatalf("reused root ref=%q want=%q", reusedRef, rootRef)
	}

	if err := os.WriteFile(publishedOverlay, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil, false, nil); err == nil {
		t.Fatal("inconsistent existing final was silently replaced")
	}
	if got, err := os.ReadFile(publishedOverlay); err != nil || string(got) != "partial" {
		t.Fatalf("inconsistent final changed: %q err=%v", got, err)
	}
}

func TestPublishLocalToLocationRejectsSnapshotParentDigestMismatch(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	parentCfg := &SnapshotCfg{}
	parentCfg.Resources.Capacity.Memory = "4KiB"
	parentPath := writePublishSnapshot(t, sourceDir, parentCfg)

	rootCfg := &SnapshotCfg{FromRefs: []string{
		"file://" + filepath.Base(parentPath) + "@sha256:" + strings.Repeat("f", 64),
	}}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootPath := writePublishSnapshot(t, sourceDir, rootCfg)

	_, err := PublishLocalToLocation(ctx, rootPath, "shared", t.TempDir(), nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("publish error = %v, want snapshot parent digest mismatch", err)
	}
}

func TestPublishLocalToLocationRejectsExistingSymlinkDestination(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	overlayPath, overlayDigest := writePublishArtifact(t, sourceDir, ".overlay", bytes.Repeat([]byte{0xA5}, 4096))
	snapCfg := &SnapshotCfg{}
	snapCfg.Resources.Capacity.Memory = "4KiB"
	snapCfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: "file://" + filepath.Base(overlayPath)}
	rootPath := writePublishSnapshot(t, sourceDir, snapCfg)

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(targetDir, strings.TrimPrefix(overlayDigest, "sha256:")+".overlay")
	if err := os.Symlink(victim, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil, false, nil); err == nil {
		t.Fatal("existing symlink destination was followed")
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "partial" {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
	info, err := os.Lstat(destination)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination symlink changed: info=%v err=%v", info, err)
	}
}

func TestPublishLocationFileCleansUpOwnedPartialFinal(t *testing.T) {
	dir := t.TempDir()
	source, identity := writePublishArtifact(t, t.TempDir(), ".overlay", bytes.Repeat([]byte{0x42}, 4096))
	scheme, digest, _ := strings.Cut(identity, ":")
	p := newSnapshotPublisher(context.Background(), nil, false, nil)
	p.location, p.directory = "shared", dir

	originalCopy := publishLocationCopy
	publishLocationCopy = func(_ context.Context, destination io.Writer, source io.Reader) (int64, error) {
		buf := make([]byte, 32)
		n, _ := source.Read(buf)
		written, _ := destination.Write(buf[:n])
		return int64(written), errors.New("injected copy failure")
	}
	t.Cleanup(func() { publishLocationCopy = originalCopy })

	if _, err := p.publishLocationFile(source, ".overlay", scheme, digest, 4096); err == nil || !strings.Contains(err.Error(), "injected copy failure") {
		t.Fatalf("publish error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, digest+".overlay")); !os.IsNotExist(err) {
		t.Fatalf("partial final was not removed: %v", err)
	}
}

func TestPublishLocationFileConcurrentPublishers(t *testing.T) {
	dir := t.TempDir()
	source, identity := writePublishArtifact(t, t.TempDir(), ".overlay", bytes.Repeat([]byte{0x37}, 4096))
	scheme, digest, _ := strings.Cut(identity, ":")
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			p := newSnapshotPublisher(context.Background(), nil, false, nil)
			p.location, p.directory = "shared", dir
			<-start
			_, err := p.publishLocationFile(source, ".overlay", scheme, digest, 4096)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	if err := validatePublishedFinal(context.Background(), filepath.Join(dir, digest+".overlay"), 4096, nil, false, scheme, digest); err != nil {
		t.Fatalf("final validation: %v", err)
	}
}

func TestPublishLocationFileDoesNotReplaceNonRegularFinal(t *testing.T) {
	dir := t.TempDir()
	source, identity := writePublishArtifact(t, t.TempDir(), ".overlay", bytes.Repeat([]byte{0x19}, 4096))
	scheme, digest, _ := strings.Cut(identity, ":")
	destination := filepath.Join(dir, digest+".overlay")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	p := newSnapshotPublisher(context.Background(), nil, false, nil)
	p.location, p.directory = "shared", dir
	if _, err := p.publishLocationFile(source, ".overlay", scheme, digest, 4096); err == nil {
		t.Fatal("existing directory was accepted or replaced")
	}
	if info, err := os.Stat(destination); err != nil || !info.IsDir() {
		t.Fatalf("existing directory changed: info=%v err=%v", info, err)
	}
}

func TestPublishLocalToLocationPreservesSameLocationBoundary(t *testing.T) {
	ctx := context.Background()
	targetDir := t.TempDir()
	boundaryRef := "file://" + strings.Repeat("a", 64) + ".overlay@location:shared"
	rootCfg := &SnapshotCfg{}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootCfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: boundaryRef}
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)

	rootRef, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := manifest.ParseRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	stream, _, _, err := openTarArtifact(filepath.Join(targetDir, parsed.Path), manifest.Ref{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_, publishedCfg, err := readSnapshotEntries(ctx, stream, int64(stream.Size()))
	stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	if publishedCfg.Boot.Root.Overlay.Base != boundaryRef {
		t.Fatalf("same-location boundary = %q, want %q", publishedCfg.Boot.Root.Overlay.Base, boundaryRef)
	}
}

func TestSnapshotPublisherChecksPreservedManifestRefs(t *testing.T) {
	p := newSnapshotPublisher(context.Background(), nil, false, nil)
	p.fetcher = failingManifestFetcher{}
	ref := "manifest://" + strings.Repeat("a", 64)
	_, err := p.publishRef("parent", ref, "", publishLeafArtifact)
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("publishRef error = %v, want manifest backend check", err)
	}
}

type failingManifestFetcher struct{}

func (failingManifestFetcher) OpenManifest(context.Context, store.ContentKey) (fetch.Stream, error) {
	return nil, errors.New("backend unavailable")
}

func TestPreflightChecksManifestArtifacts(t *testing.T) {
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: "manifest://" + strings.Repeat("a", 64)}
	path := writePublishSnapshot(t, t.TempDir(), cfg)
	err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: path,
		Fetcher:      failingManifestFetcher{},
	})
	if err == nil || !strings.Contains(err.Error(), "backend unavailable") {
		t.Fatalf("manifest preflight error = %v", err)
	}
}

func TestPreflightLocatedRefsTreatsRootMemoryListAsAuthoritative(t *testing.T) {
	parentDir := t.TempDir()
	ancestorCfg := &SnapshotCfg{}
	ancestorCfg.Resources.Capacity.Memory = "4KiB"
	ancestorPath := writePublishSnapshot(t, parentDir, ancestorCfg)
	parentCfg := &SnapshotCfg{FromRefs: []string{
		"file://" + filepath.Base(ancestorPath),
		"file://" + strings.Repeat("d", 64) + ".snapshot@location:stale-parent",
	}}
	parentCfg.Resources.Capacity.Memory = "4KiB"
	parentCfg.Boot.Root.Overlay = &SnapOverlayCfg{
		Base: "file://" + strings.Repeat("b", 64) + ".overlay@location:ignored-parent-disk",
	}
	parentPath := writePublishSnapshot(t, parentDir, parentCfg)

	rootDir := t.TempDir()
	rootCfg := &SnapshotCfg{FromRefs: []string{
		"file://" + filepath.Base(parentPath) + "@location:parent",
		"file://" + filepath.Base(ancestorPath) + "@location:parent",
	}}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootPath := writePublishSnapshot(t, rootDir, rootCfg)

	if err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: rootPath,
		RefLocations: config.RefLocations{"parent": parentDir},
	}); err != nil {
		t.Fatalf("flattened root memory list was not authoritative: %v", err)
	}
}

func TestPreflightLocatedRefsValidatesOpaqueRootMemoryLayers(t *testing.T) {
	rootCfg := &SnapshotCfg{FromRefs: []string{
		"file://" + strings.Repeat("d", 64) + ".snapshot@location:missing-parent",
	}}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)

	err := preflightLocatedRefs(context.Background(), Options{SnapshotPath: rootPath})
	if err == nil || !strings.Contains(err.Error(), `location "missing-parent" is not configured`) {
		t.Fatalf("missing opaque root memory layer error = %v", err)
	}
}

func TestPreflightLocatedRefsBoundsFlattenedMemoryLayers(t *testing.T) {
	refs := make([]string, maxSnapshotParentEntries+1)
	for i := range refs {
		refs[i] = "file://" + fmt.Sprintf("%064x", i+1) + ".snapshot"
	}
	rootCfg := &SnapshotCfg{FromRefs: refs}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)

	err := preflightLocatedRefs(context.Background(), Options{SnapshotPath: rootPath})
	if err == nil || !strings.Contains(err.Error(), "snapshot parent graph exceeds 1024 entries") {
		t.Fatalf("oversized flattened memory list error = %v", err)
	}
}

func TestPreflightLocatedRefsIgnoresParentDiskArtifacts(t *testing.T) {
	parentDir := t.TempDir()
	parentCfg := &SnapshotCfg{}
	parentCfg.Resources.Capacity.Memory = "4KiB"
	parentCfg.Boot.Root.Overlay = &SnapOverlayCfg{
		Base: "file://" + strings.Repeat("b", 64) + ".overlay@location:obsolete-parent-disk",
	}
	parentPath := writePublishSnapshot(t, parentDir, parentCfg)

	rootCfg := &SnapshotCfg{FromRefs: []string{
		"file://" + filepath.Base(parentPath) + "@location:parent",
	}}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)

	if err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: rootPath,
		RefLocations: config.RefLocations{"parent": parentDir},
	}); err != nil {
		t.Fatalf("parent disk artifact affected memory preflight: %v", err)
	}
}

func TestPreflightLocatedRefsValidatesDiskArtifactIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		makeRef func(t *testing.T, locationDir, artifactPath, digest string) string
		want    string
	}{
		{
			name: "digest qualifier",
			makeRef: func(_ *testing.T, _, artifactPath, _ string) string {
				return "file://" + filepath.Base(artifactPath) + "@sha256:" + strings.Repeat("f", 64) + "@location:shared"
			},
			want: "digest mismatch",
		},
		{
			name: "content addressed name",
			makeRef: func(t *testing.T, locationDir, artifactPath, _ string) string {
				wrongPath := filepath.Join(locationDir, "wrong.overlay")
				if err := os.Rename(artifactPath, wrongPath); err != nil {
					t.Fatal(err)
				}
				return "file://wrong.overlay@location:shared"
			},
			want: "content name does not match",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locationDir := t.TempDir()
			artifactPath, digest := writePublishArtifact(t, locationDir, ".overlay", bytes.Repeat([]byte{0x4B}, 4096))
			rootCfg := &SnapshotCfg{}
			rootCfg.Resources.Capacity.Memory = "4KiB"
			rootCfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: tc.makeRef(t, locationDir, artifactPath, digest)}
			rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)
			err := preflightLocatedRefs(context.Background(), Options{
				SnapshotPath: rootPath,
				RefLocations: config.RefLocations{"shared": locationDir},
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("preflight error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPreflightLocatedRefsValidatesRootIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		makeRef func(t *testing.T, rootPath string) (string, string)
	}{
		{
			name: "digest qualifier",
			makeRef: func(_ *testing.T, rootPath string) (string, string) {
				return "file://" + filepath.Base(rootPath) + "@sha256:" + strings.Repeat("f", 64) + "@location:shared", "digest mismatch"
			},
		},
		{
			name: "content addressed name",
			makeRef: func(t *testing.T, rootPath string) (string, string) {
				wrongPath := filepath.Join(filepath.Dir(rootPath), "wrong.snapshot")
				if err := os.Rename(rootPath, wrongPath); err != nil {
					t.Fatal(err)
				}
				return "file://wrong.snapshot@location:shared", "content name does not match"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locationDir := t.TempDir()
			rootCfg := &SnapshotCfg{}
			rootCfg.Resources.Capacity.Memory = "4KiB"
			rootPath := writePublishSnapshot(t, locationDir, rootCfg)
			rootRef, want := tc.makeRef(t, rootPath)
			resolved, err := (config.RefLocations{"shared": locationDir}).ResolveFile(mustParseRef(t, rootRef), "")
			if err != nil {
				t.Fatal(err)
			}
			err = preflightLocatedRefs(context.Background(), Options{
				SnapshotPath: resolved,
				SnapshotRef:  rootRef,
				RefLocations: config.RefLocations{"shared": locationDir},
			})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("preflight root error = %v, want %q", err, want)
			}
		})
	}
}

func TestPreflightLocatedRefsFollowsRootSymlink(t *testing.T) {
	targetCfg := &SnapshotCfg{}
	targetCfg.Resources.Capacity.Memory = "4KiB"
	targetPath := writePublishSnapshot(t, t.TempDir(), targetCfg)
	locationDir := t.TempDir()
	linkPath := filepath.Join(locationDir, filepath.Base(targetPath))
	if err := os.Symlink(targetPath, linkPath); err != nil {
		t.Fatal(err)
	}
	rootRef := "file://" + filepath.Base(linkPath) + "@location:shared"
	if err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: linkPath,
		SnapshotRef:  rootRef,
		RefLocations: config.RefLocations{"shared": locationDir},
	}); err != nil {
		t.Fatalf("preflight located root symlink: %v", err)
	}
}

func TestPreflightLocatedRefsIncludesHostOverrides(t *testing.T) {
	artifactPath, digest := writePublishArtifact(t, t.TempDir(), ".erofs", bytes.Repeat([]byte{0x6A}, 4096))
	rootCfg := &SnapshotCfg{}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootCfg.Boot.Root.BaseRef = "file://" + filepath.Base(artifactPath) + "@sha256:" + strings.TrimPrefix(digest, "sha256:")
	rootCfg.Boot.Root.Overlay = &SnapOverlayCfg{}
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)
	hostCfg := &config.SandboxConfig{}
	hostCfg.Boot.Root.Base = "file://" + filepath.Base(artifactPath) + "@location:platform"
	err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: rootPath,
		HostCfg:      hostCfg,
	})
	if err == nil || !strings.Contains(err.Error(), `location "platform" is not configured`) {
		t.Fatalf("host override preflight error = %v", err)
	}
}

func TestPreflightSkipsBaseRefsReplacedByHostOverrides(t *testing.T) {
	rootBase, rootDigest := writePublishArtifact(t, t.TempDir(), ".erofs", bytes.Repeat([]byte{0x71}, 4096))
	diskBase, diskDigest := writePublishArtifact(t, t.TempDir(), ".erofs", bytes.Repeat([]byte{0x72}, 4096))
	rootCfg := &SnapshotCfg{}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootCfg.Boot.Root.BaseRef = "file://" + filepath.Base(rootBase) + "@sha256:" + strings.TrimPrefix(rootDigest, "sha256:") + "@location:missing-root"
	rootCfg.Boot.Root.Overlay = &SnapOverlayCfg{}
	rootCfg.Boot.Disks = []SnapDiskNode{{
		BaseRef: "file://" + filepath.Base(diskBase) + "@sha256:" + strings.TrimPrefix(diskDigest, "sha256:") + "@location:missing-disk",
		Overlay: &SnapOverlayCfg{},
	}}
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)
	hostCfg := &config.SandboxConfig{}
	hostCfg.Boot.Root.Base = "file://" + rootBase
	hostCfg.Boot.Disks = []config.DiskConfig{{RootConfig: config.RootConfig{
		Base:    "file://" + diskBase,
		Overlay: &config.OverlayConfig{},
	}}}

	if err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: rootPath,
		HostCfg:      hostCfg,
	}); err != nil {
		t.Fatalf("preflight with host base overrides: %v", err)
	}
}

func mustParseRef(t *testing.T, raw string) manifest.Ref {
	t.Helper()
	ref, err := manifest.ParseRef(raw)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestLocalSnapshotPathKeepsUnlocatedDigestRef(t *testing.T) {
	path := "/snapshots/" + strings.Repeat("a", 64) + ".snapshot"
	localRef := "file://" + path + "@sha256:" + strings.Repeat("a", 64)
	if got := (Options{SnapshotPath: path, SnapshotRef: localRef}).localSnapshotPath(); got != path {
		t.Fatalf("localSnapshotPath = %q, want %q", got, path)
	}
	locatedRef := "file://" + filepath.Base(path) + "@location:shared"
	if got := (Options{SnapshotPath: path, SnapshotRef: locatedRef}).localSnapshotPath(); got != "" {
		t.Fatalf("located localSnapshotPath = %q, want empty", got)
	}
}

func TestOpenSnapshotArtifactTreatsPlainPathLiterally(t *testing.T) {
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	path := writePublishSnapshot(t, t.TempDir(), cfg)
	dir := filepath.Join(t.TempDir(), "literal@location:not-a-ref")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	literal := filepath.Join(dir, filepath.Base(path))
	if err := os.Rename(path, literal); err != nil {
		t.Fatal(err)
	}
	stream, scheme, digest, err := openSnapshotArtifact(context.Background(), Options{SnapshotPath: literal})
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := stream.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if scheme != tarstream.DigestSchemeSHA256 || digest == "" {
		t.Fatalf("literal snapshot identity = %s:%s", scheme, digest)
	}
}

func TestFileSnapshotRefUsesActualSchemeAndPreservesLocation(t *testing.T) {
	target := filepath.Join(t.TempDir(), strings.Repeat("a", 64)+".snapshot")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "sid.snapshot")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	hmacDigest := strings.Repeat("b", 64)
	got, err := fileSnapshotRef(link, "", tarstream.DigestSchemeHMAC, hmacDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := mustParseRef(t, got)
	if ref.Path != filepath.Base(target) || ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest != hmacDigest {
		t.Fatalf("local self ref=%#v", ref)
	}
	locatedRaw := "file://logical.snapshot@sha256:" + strings.Repeat("c", 64) + "@location:shared"
	got, err = fileSnapshotRef(link, locatedRaw, tarstream.DigestSchemeHMAC, hmacDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref = mustParseRef(t, got)
	if ref.Path != "logical.snapshot" || ref.Location != "shared" || ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest != hmacDigest {
		t.Fatalf("located self ref=%#v", ref)
	}
}

func TestResolveLocalMergePathUsesRefLocations(t *testing.T) {
	snapshotPath := "/snapshots/root.snapshot"
	locations := config.RefLocations{"shared": "/shared/location"}
	for _, tc := range []struct {
		ref  string
		want string
	}{
		{"file://layer.overlay", "/snapshots/layer.overlay"},
		{"file://layer.overlay@location:shared", "/shared/location/layer.overlay"},
		{"manifest://" + strings.Repeat("a", 64), ""},
	} {
		got, err := resolveLocalMergePath(tc.ref, snapshotPath, locations)
		if err != nil {
			t.Fatalf("resolveLocalMergePath(%q): %v", tc.ref, err)
		}
		if got != tc.want {
			t.Fatalf("resolveLocalMergePath(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestResolveAnyRefPreservesLocatedBase(t *testing.T) {
	locationDir := t.TempDir()
	path, digest := writePublishArtifact(t, locationDir, ".erofs", bytes.Repeat([]byte{0x73}, 4096))
	ref := mustParseRef(t, "file://"+filepath.Base(path)+"@sha256:"+strings.TrimPrefix(digest, "sha256:")+"@location:shared")
	got, err := resolveAnyRef("", ref, "/snapshots/root.snapshot", "boot.root.base", config.RefLocations{"shared": locationDir}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != ref.String() {
		t.Fatalf("resolved located base = %q, want %q", got, ref.String())
	}
}

func writePublishArtifact(t *testing.T, dir, ext string, payload []byte) (string, string) {
	return writePublishNamedArtifact(t, dir, ext, "payload", payload)
}

func writePublishNamedArtifact(t *testing.T, dir, ext, name string, payload []byte) (string, string) {
	t.Helper()
	tmp, err := os.CreateTemp(dir, "artifact-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), tmp, name, sparse.Dense(bytes.NewReader(payload), uint64(len(payload))))
	if err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, digest+ext)
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	return path, scheme + ":" + digest
}

func writePublishSnapshot(t *testing.T, dir string, cfg *SnapshotCfg) string {
	t.Helper()
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	zipBody, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  []byte("{}"),
		"state.json":   []byte("{}"),
		"snapshot.cfg": body,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := append(make([]byte, 4096), zipBody...)
	path, _ := writePublishNamedArtifact(t, dir, ".snapshot", "snapshot", payload)
	return path
}
