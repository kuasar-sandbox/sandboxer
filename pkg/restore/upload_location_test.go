package restore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"gopkg.in/yaml.v3"
)

func TestPublishLocalToLocationRewritesLocalRefsAndRepairsPartialFile(t *testing.T) {
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

	rootRef, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil)
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

	rootStream, _, err := openTarArtifact(filepath.Join(targetDir, parsedRoot.Path))
	if err != nil {
		t.Fatal(err)
	}
	_, publishedCfg, err := readSnapshotEntries(ctx, rootStream, int64(rootStream.Size()))
	rootStream.Close()
	if err != nil {
		t.Fatal(err)
	}
	wantOverlayRef := "file://" + filepath.Base(publishedOverlay) + "@location:shared"
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

	if err := os.WriteFile(publishedOverlay, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	secondRef, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if secondRef != rootRef {
		t.Fatalf("repeat root ref = %q, want %q", secondRef, rootRef)
	}
	if err := verifyArtifactFile(publishedOverlay, overlayDigest, 4096); err != nil {
		t.Fatalf("partial file was not repaired: %v", err)
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

	_, err := PublishLocalToLocation(ctx, rootPath, "shared", t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 marker mismatch") {
		t.Fatalf("publish error = %v, want snapshot parent digest mismatch", err)
	}
}

func TestPublishLocalToLocationRefusesSymlinkDestination(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	overlayPath, overlayDigest := writePublishArtifact(t, sourceDir, ".overlay", bytes.Repeat([]byte{0xA5}, 4096))
	snapCfg := &SnapshotCfg{}
	snapCfg.Resources.Capacity.Memory = "4KiB"
	snapCfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: "file://" + filepath.Base(overlayPath)}
	rootPath := writePublishSnapshot(t, sourceDir, snapCfg)

	victim := filepath.Join(t.TempDir(), "victim")
	const original = "do not overwrite"
	if err := os.WriteFile(victim, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(targetDir, strings.TrimPrefix(overlayDigest, "sha256:")+".overlay")
	if err := os.Symlink(victim, destination); err != nil {
		t.Fatal(err)
	}
	_, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil)
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("publish error = %v, want symlink rejection", err)
	}
	body, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != original {
		t.Fatalf("victim content = %q", body)
	}
}

func TestPublishLocalToLocationValidatesSameLocationBoundary(t *testing.T) {
	ctx := context.Background()
	targetDir := t.TempDir()
	artifactPath, _ := writePublishArtifact(t, targetDir, ".overlay", bytes.Repeat([]byte{0x3C}, 4096))
	boundaryRef := "file://" + filepath.Base(artifactPath) + "@location:shared"
	rootCfg := &SnapshotCfg{}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootCfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: boundaryRef}
	rootPath := writePublishSnapshot(t, t.TempDir(), rootCfg)

	if _, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil); err != nil {
		t.Fatalf("valid same-location boundary: %v", err)
	}
	outsidePath := filepath.Join(t.TempDir(), filepath.Base(artifactPath))
	if err := os.Rename(artifactPath, outsidePath); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil); err == nil || !strings.Contains(err.Error(), boundaryRef) {
		t.Fatalf("missing same-location boundary error = %v", err)
	}
	if err := os.Symlink(outsidePath, artifactPath); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLocalToLocation(ctx, rootPath, "shared", targetDir, nil); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink same-location boundary error = %v", err)
	}
}

func TestSnapshotPublisherChecksPreservedManifestRefs(t *testing.T) {
	p := newSnapshotPublisher(context.Background(), nil)
	p.manifestConfig = &manifest.Config{}
	ref := "manifest://" + strings.Repeat("a", 64)
	_, err := p.publishRef("parent", ref, "", false)
	if err == nil || !strings.Contains(err.Error(), "cache.endpoint or store.endpoint required") {
		t.Fatalf("publishRef error = %v, want manifest backend check", err)
	}
}

func TestPreflightLocatedRefsWalksMemoryParents(t *testing.T) {
	parentDir := t.TempDir()
	parentCfg := &SnapshotCfg{}
	parentCfg.Resources.Capacity.Memory = "4KiB"
	parentCfg.Boot.Root.BaseRef = "manifest://" + strings.Repeat("a", 64)
	parentCfg.Boot.Root.Overlay = &SnapOverlayCfg{
		Base: "file://" + strings.Repeat("b", 64) + ".overlay@location:missing",
	}
	parentPath := writePublishSnapshot(t, parentDir, parentCfg)

	rootDir := t.TempDir()
	rootCfg := &SnapshotCfg{FromRefs: []string{
		"file://" + filepath.Base(parentPath) + "@location:parent",
	}}
	rootCfg.Resources.Capacity.Memory = "4KiB"
	rootCfg.Boot.Root.BaseRef = "manifest://" + strings.Repeat("c", 64)
	rootPath := writePublishSnapshot(t, rootDir, rootCfg)

	err := preflightLocatedRefs(context.Background(), Options{
		SnapshotPath: rootPath,
		RefLocations: config.RefLocations{"parent": parentDir},
	})
	if err == nil || !strings.Contains(err.Error(), `location "missing" is not configured`) {
		t.Fatalf("preflight error = %v", err)
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
			want: "sha256 marker mismatch",
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
			want: "does not match marker",
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

func writePublishArtifact(t *testing.T, dir, ext string, payload []byte) (string, string) {
	t.Helper()
	tmp, err := os.CreateTemp(dir, "artifact-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := tarstream.WriteTo(context.Background(), tmp, "payload", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))))
	if err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")+ext)
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	return path, digest
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
	path, _ := writePublishArtifact(t, dir, ".snapshot", payload)
	return path
}
