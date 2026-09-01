package artifact

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"golang.org/x/sys/unix"
)

func bundlePublishFixture(t *testing.T, cfg *manifest.Config, storage *ProcessStorage) (string, string) {
	return bundlePublishFixtureWithRefs(t, cfg, storage, nil)
}

func bundlePublishFixtureWithRefs(t *testing.T, cfg *manifest.Config, storage *ProcessStorage, refs []string) (string, string) {
	t.Helper()
	directory := t.TempDir()
	ctx := context.Background()
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := snapshot.NewPlannedBundleSink(directory, "exact", cfg, storage.CustomerKeyFunc(), admission, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	runtimeConfig, _ := publishPortable(t, "")
	sandboxSource, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x41}, 8192)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	sandboxRef, _, err := sink.AbsorbSandbox(ctx, sandboxSource)
	if err != nil {
		t.Fatal(err)
	}
	snapshotConfig, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: sandboxRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshotSource, err := snapshotfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x52}, 16*1024)), []byte("{}"), []byte("{}"), snapshotConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, _, err := sink.AbsorbSnapshot(ctx, snapshotSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(ctx, rootRef, ""); err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(strings.TrimPrefix(rootRef, "manifest://"))
	if err != nil {
		t.Fatal(err)
	}
	return rootRef, filepath.Join(directory, manifest.HexKey(root)+".bundle")
}

func overlayBundleFixture(t *testing.T, cfg *manifest.Config, storage *ProcessStorage, directory string) (string, string) {
	t.Helper()
	ctx := context.Background()
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := snapshot.NewPlannedBundleSink(
		directory, "external", cfg, storage.CustomerKeyFunc(), admission, nil, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	ref, _, err := sink.AbsorbOverlay(
		ctx, bytes.NewReader(bytes.Repeat([]byte{0x39}, 8192)), nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSandbox(ctx, ref, ""); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := manifest.ParseKeyRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	return ref, filepath.Join(directory, manifest.HexKey(key)+".bundle")
}

func unavailableBundleFallbackFixture(t *testing.T) (*ProcessStorage, string, string, []byte) {
	t.Helper()
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	keyFn := storage.CustomerKeyFunc()

	externalDirectory := t.TempDir()
	externalRef, externalPath := overlayBundleFixture(t, cfg, storage, externalDirectory)
	externalName := "fallback.bundle"
	externalBytes, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatal(err)
	}
	rootDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootDirectory, externalName), externalBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	rootAdmission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rootSink, err := snapshot.NewPlannedBundleSink(
		rootDirectory, "root", cfg, keyFn, rootAdmission,
		[]string{"file://missing.bundle", "file://" + externalName}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, _ := publishPortable(t, externalRef)
	rootSource, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x4a}, 8192)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, _, err := rootSink.AbsorbSandbox(ctx, rootSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootSink.CommitSandbox(ctx, rootRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := rootSink.Close(); err != nil {
		t.Fatal(err)
	}
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(rootDirectory, manifest.HexKey(rootKey)+".bundle")
	return storage, rootPath, externalName, externalBytes
}

func TestLocationPublisherCopiesBundleExactly(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	rootRef, sourcePath := bundlePublishFixture(t, cfg, storage)
	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	targetDirectory := t.TempDir()
	publisher, err := NewLocationPublisher(storage, "shared", targetDirectory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs := newTrackingLocationFileSystem()
	publisher.target.(*locationPublishTarget).fs = fs
	oldUmask := unix.Umask(0o077)
	t.Cleanup(func() { unix.Umask(oldUmask) })
	result, publishErr := publisher.Publish(ctx, sourcePath)
	unix.Umask(oldUmask)
	if publishErr != nil {
		t.Fatalf("publish Bundle: %v", publishErr)
	}
	if result.Role != RoleSnapshot {
		t.Fatalf("published role = %q", result.Role)
	}
	root := strings.TrimPrefix(rootRef, "manifest://")
	wantRef := "file://" + root + ".bundle@manifest:" + root + "@location:shared"
	if result.Ref != wantRef {
		t.Fatalf("published ref = %q, want %q", result.Ref, wantRef)
	}
	entries, err := os.ReadDir(targetDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != root+".bundle" {
		t.Fatalf("named location entries = %v", entries)
	}
	targetBytes, err := os.ReadFile(filepath.Join(targetDirectory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(targetBytes, sourceBytes) {
		t.Fatal("named location Bundle is not an exact copy")
	}
	targetInfo, err := os.Stat(filepath.Join(targetDirectory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if targetInfo.Mode().Perm() != 0o644 {
		t.Fatalf("located Bundle mode = %o, want 644", targetInfo.Mode().Perm())
	}
	info, err := storage.Inspect(ctx, result.Ref, config.RefLocations{"shared": targetDirectory})
	if err != nil {
		t.Fatalf("official opener rejected located Bundle: %v", err)
	}
	if info.Role != RoleSnapshot || info.Snapshot.SandboxRef == "" {
		t.Fatalf("located Bundle root = %+v", info)
	}
	if fs.creates.Load() != 1 || fs.written.Load() != int64(len(sourceBytes)) {
		t.Fatalf("shared target creates=%d write-bytes=%d, want one exact write of %d",
			fs.creates.Load(), fs.written.Load(), len(sourceBytes))
	}
	before, err := os.Stat(filepath.Join(targetDirectory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	writes := fs.written.Load()
	reused, err := publisher.Publish(ctx, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(targetDirectory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if reused.Ref != result.Ref || !os.SameFile(before, after) || fs.written.Load() != writes {
		t.Fatalf("Bundle reuse result=%+v same-inode=%t writes=%d->%d", reused, os.SameFile(before, after), writes, fs.written.Load())
	}
	for _, alias := range []string{"exact.snapshot", "exact.sandbox"} {
		if _, err := os.Lstat(filepath.Join(targetDirectory, alias)); !os.IsNotExist(err) {
			t.Fatalf("named location created alias %q: %v", alias, err)
		}
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLocationPublisherCopiesPinnedBundleSource(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	rootRef, sourcePath := bundlePublishFixture(t, cfg, storage)
	original, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	replacementRef, replacementPath := bundlePublishFixtureWithRefs(
		t, cfg, storage, []string{"file://missing.bundle"},
	)
	if replacementRef != rootRef {
		t.Fatalf("replacement root = %q, want %q", replacementRef, rootRef)
	}
	replacement, err := os.ReadFile(replacementPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, replacement) {
		t.Fatal("replacement Bundle fixture unexpectedly matches original bytes")
	}

	targetDirectory := t.TempDir()
	publisher, err := NewLocationPublisher(storage, "shared", targetDirectory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fs := newTrackingLocationFileSystem()
	var replaceOnce sync.Once
	fs.wrapCreate = func(_ string, created locationWriteFile) locationWriteFile {
		replaceOnce.Do(func() {
			if err := os.Rename(replacementPath, sourcePath); err != nil {
				t.Fatalf("replace Bundle source: %v", err)
			}
		})
		return created
	}
	publisher.target.(*locationPublishTarget).fs = fs
	result, publishErr := publisher.Publish(ctx, sourcePath)
	closeErr := publisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish atomically replaced Bundle err=%v close=%v", publishErr, closeErr)
	}
	ref, err := manifest.ParseRef(result.Ref)
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(filepath.Join(targetDirectory, ref.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, original) {
		t.Fatal("publisher copied a path replacement instead of the pinned Bundle source")
	}
	currentSource, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(currentSource, replacement) {
		t.Fatal("test did not replace the source path during publication")
	}
	info, err := storage.Inspect(ctx, result.Ref, config.RefLocations{"shared": targetDirectory})
	if err != nil {
		t.Fatalf("official opener rejected pinned Bundle publication: %v", err)
	}
	if info.Role != RoleSnapshot {
		t.Fatalf("published graph role = %q, want snapshot", info.Role)
	}
}

func TestLocationPublisherCanonicalizesExplicitlySelectedBundleName(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	rootRef, sourcePath := bundlePublishFixture(t, cfg, storage)
	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(t.TempDir(), "release.bundle")
	if err := os.WriteFile(customPath, sourceBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	root := strings.TrimPrefix(rootRef, "manifest://")
	input := "file://" + customPath + "@manifest:" + root
	targetDirectory := t.TempDir()
	publisher, err := NewLocationPublisher(storage, "shared", targetDirectory, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, publishErr := publisher.Publish(ctx, input)
	closeErr := publisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish explicitly selected Bundle err=%v close=%v", publishErr, closeErr)
	}
	wantRef := "file://" + root + ".bundle@manifest:" + root + "@location:shared"
	if result.Ref != wantRef {
		t.Fatalf("published ref = %q, want %q", result.Ref, wantRef)
	}
	published, err := os.ReadFile(filepath.Join(targetDirectory, root+".bundle"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, sourceBytes) {
		t.Fatal("explicitly selected Bundle was not copied exactly")
	}
}

func TestLocationPublisherPreservesLocatedBundleDependency(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	keyFn := storage.CustomerKeyFunc()

	externalDirectory := t.TempDir()
	externalRef, externalPath := overlayBundleFixture(t, cfg, storage, externalDirectory)
	externalName := filepath.Base(externalPath)

	rootDirectory := t.TempDir()
	rootAdmission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	externalBundleRef := "file://" + externalName + "@location:external"
	rootSink, err := snapshot.NewPlannedBundleSink(
		rootDirectory, "root", cfg, keyFn, rootAdmission, []string{externalBundleRef}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, _ := publishPortable(t, externalRef)
	rootSource, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x4a}, 8192)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, _, err := rootSink.AbsorbSandbox(ctx, rootSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootSink.CommitSandbox(ctx, rootRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := rootSink.Close(); err != nil {
		t.Fatal(err)
	}
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	rootName := manifest.HexKey(rootKey) + ".bundle"
	rootPath := filepath.Join(rootDirectory, rootName)

	targetDirectory := t.TempDir()
	locations := config.RefLocations{
		"external":  externalDirectory,
		"published": targetDirectory,
	}
	publisher, err := NewLocationPublisher(storage, "published", targetDirectory, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, publishErr := publisher.Publish(ctx, rootPath)
	closeErr := publisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish Bundle with located dependency err=%v close=%v", publishErr, closeErr)
	}
	if result.Role != RoleSandbox {
		t.Fatalf("published role = %q, want sandbox", result.Role)
	}
	wantRef := "file://" + rootName + "@manifest:" + manifest.HexKey(rootKey) + "@location:published"
	if result.Ref != wantRef {
		t.Fatalf("published ref = %q, want %q", result.Ref, wantRef)
	}
	entries, err := os.ReadDir(targetDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != rootName {
		t.Fatalf("published location entries = %v, want only root Bundle", entries)
	}
	if _, err := os.Lstat(filepath.Join(targetDirectory, externalName)); !os.IsNotExist(err) {
		t.Fatalf("located dependency was copied into target: %v", err)
	}
	info, err := storage.Inspect(ctx, result.Ref, locations)
	if err != nil {
		t.Fatalf("official opener rejected published graph: %v", err)
	}
	if info.Role != RoleSandbox {
		t.Fatalf("published graph role = %q, want sandbox", info.Role)
	}
}

func TestLocationPublisherSkipsUnavailableBundleFallback(t *testing.T) {
	ctx := context.Background()
	storage, rootPath, externalName, _ := unavailableBundleFallbackFixture(t)

	targetDirectory := t.TempDir()
	locations := config.RefLocations{"published": targetDirectory}
	publisher, err := NewLocationPublisher(storage, "published", targetDirectory, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, publishErr := publisher.Publish(ctx, rootPath)
	closeErr := publisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish Bundle with unavailable fallback err=%v close=%v", publishErr, closeErr)
	}
	entries, err := os.ReadDir(targetDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("published location entries = %v, want available dependency and root", entries)
	}
	if _, err := os.Lstat(filepath.Join(targetDirectory, "missing.bundle")); !os.IsNotExist(err) {
		t.Fatalf("unavailable fallback was materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDirectory, externalName)); err != nil {
		t.Fatalf("available fallback was not copied: %v", err)
	}
	info, err := storage.Inspect(ctx, result.Ref, locations)
	if err != nil {
		t.Fatalf("official opener rejected fallback graph: %v", err)
	}
	if info.Role != RoleSandbox {
		t.Fatalf("published graph role = %q, want sandbox", info.Role)
	}
}

func TestLocationPublisherRejectsTargetThatChangesUnavailableFallback(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "preexisting"
		if concurrent {
			name = "concurrent"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			storage, rootPath, _, _ := unavailableBundleFallbackFixture(t)
			targetDirectory := t.TempDir()
			unexpected := []byte("not a Bundle")
			unexpectedPath := filepath.Join(targetDirectory, "missing.bundle")
			if !concurrent {
				if err := os.WriteFile(unexpectedPath, unexpected, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			publisher, err := NewLocationPublisher(
				storage, "published", targetDirectory,
				config.RefLocations{"published": targetDirectory}, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			if concurrent {
				fs := newTrackingLocationFileSystem()
				var materialize sync.Once
				fs.wrapCreate = func(_ string, created locationWriteFile) locationWriteFile {
					materialize.Do(func() {
						if err := os.WriteFile(unexpectedPath, unexpected, 0o644); err != nil {
							t.Fatalf("materialize fallback during publication: %v", err)
						}
					})
					return created
				}
				publisher.target.(*locationPublishTarget).fs = fs
			}
			result, publishErr := publisher.Publish(ctx, rootPath)
			closeErr := publisher.Close()
			if publishErr == nil || closeErr != nil {
				t.Fatalf("publish result=%+v err=%v close=%v, want publication failure", result, publishErr, closeErr)
			}
			if !strings.Contains(publishErr.Error(), "would change fallback selection") ||
				!strings.Contains(publishErr.Error(), "cleanup is required") {
				t.Fatalf("publish error = %v, want explicit cleanup/fallback error", publishErr)
			}
			current, err := os.ReadFile(unexpectedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(current, unexpected) {
				t.Fatal("publisher modified an unknown-owner fallback file")
			}
			if _, err := os.Lstat(filepath.Join(targetDirectory, filepath.Base(rootPath))); !os.IsNotExist(err) {
				t.Fatalf("publisher exposed root despite conflicting fallback: %v", err)
			}
		})
	}
}

func TestLocationPublisherPreservesLocatedBundleSourcePrecedence(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	keyFn := storage.CustomerKeyFunc()

	externalDirectory := t.TempDir()
	externalRef, externalPath := overlayBundleFixture(t, cfg, storage, externalDirectory)
	externalName := filepath.Base(externalPath)

	rootDirectory := t.TempDir()
	shadowBytes, err := os.ReadFile(externalPath)
	if err != nil {
		t.Fatal(err)
	}
	corruptBundleChunk(t, shadowBytes)
	shadowPath := filepath.Join(rootDirectory, externalName)
	if err := os.WriteFile(shadowPath, shadowBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	rootAdmission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{
		"file://" + externalName + "@location:external",
		"file://" + externalName,
	}
	rootSink, err := snapshot.NewPlannedBundleSink(
		rootDirectory, "root", cfg, keyFn, rootAdmission, refs, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfig, _ := publishPortable(t, externalRef)
	rootSource, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x4a}, 8192)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, _, err := rootSink.AbsorbSandbox(ctx, rootSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := rootSink.CommitSandbox(ctx, rootRef, ""); err != nil {
		t.Fatal(err)
	}
	if err := rootSink.Close(); err != nil {
		t.Fatal(err)
	}
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(rootDirectory, manifest.HexKey(rootKey)+".bundle")

	targetDirectory := t.TempDir()
	locations := config.RefLocations{
		"external":  externalDirectory,
		"published": targetDirectory,
	}
	publisher, err := NewLocationPublisher(storage, "published", targetDirectory, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, publishErr := publisher.Publish(ctx, rootPath)
	closeErr := publisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish Bundle with shadowed local dependency err=%v close=%v", publishErr, closeErr)
	}
	publishedShadow, err := os.ReadFile(filepath.Join(targetDirectory, externalName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(publishedShadow, shadowBytes) {
		t.Fatal("shadowed local Bundle was not copied exactly")
	}
	info, err := storage.Inspect(ctx, result.Ref, locations)
	if err != nil {
		t.Fatalf("official opener did not preserve located source precedence: %v", err)
	}
	if info.Role != RoleSandbox {
		t.Fatalf("published graph role = %q, want sandbox", info.Role)
	}
}

func corruptBundleChunk(t *testing.T, body []byte) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if !strings.HasPrefix(file.Name, "chunk/") || file.UncompressedSize64 == 0 {
			continue
		}
		offset, err := file.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		body[offset] ^= 0xff
		return
	}
	t.Fatal("Bundle fixture has no non-empty Chunk entry")
}

func TestManifestPublisherUploadsBundleExactly(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	rootRef, sourcePath := bundlePublishFixture(t, cfg, storage)
	publisher, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, publishErr := publisher.Publish(ctx, sourcePath)
	closeErr := publisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish Bundle err=%v close=%v", publishErr, closeErr)
	}
	if result.Role != RoleSnapshot || result.Ref != rootRef {
		t.Fatalf("published result = %+v, want %s", result, rootRef)
	}
	info, err := storage.Inspect(ctx, result.Ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Role != RoleSnapshot || info.Snapshot.SandboxRef == "" {
		t.Fatalf("uploaded Bundle root = %+v", info)
	}
}

func TestPublisherRejectsManifestRootInsteadOfMaterializingIt(t *testing.T) {
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	publisher := newPublisher(storage, nil, &recordingPublishTarget{}, nil)
	_, err = publisher.Publish(context.Background(), "manifest://"+strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "already portable") {
		t.Fatalf("manifest root error = %v", err)
	}
}

func bundleLocationFileFixture(t *testing.T) (locationBundleFile, []byte) {
	t.Helper()
	cfg, storage := manifestPublisherFixture(t)
	rootRef, source := bundlePublishFixture(t, cfg, storage)
	root, err := manifest.ParseKeyRef(strings.TrimPrefix(rootRef, "manifest://"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	return locationBundleFile{source: source, root: root, verifyRoot: true}, body
}

func TestLocationBundleRetriesInProgressFinal(t *testing.T) {
	file, sourceBytes := bundleLocationFileFixture(t)
	directory := t.TempDir()
	file.destination = filepath.Join(directory, filepath.Base(file.source))
	started := make(chan struct{})
	release := make(chan struct{})
	aFS := newTrackingLocationFileSystem()
	aFS.wrapCreate = func(_ string, created locationWriteFile) locationWriteFile {
		var once sync.Once
		return &hookedLocationWriteFile{base: created, writeHook: func(body []byte) (int, error) {
			first := false
			once.Do(func() { first = true })
			if !first {
				return created.Write(body)
			}
			limit := max(1, len(body)/2)
			n, err := created.Write(body[:limit])
			close(started)
			<-release
			return n, err
		}}
	}
	a := locationTestTarget(directory, nil, false)
	a.fs = aFS
	a.retry = locationRetryPolicy{window: 2 * time.Second, initial: 2 * time.Millisecond, maximum: 20 * time.Millisecond}

	bFS := newTrackingLocationFileSystem()
	retried := make(chan struct{})
	var retriedOnce sync.Once
	bFS.onOpen = func(count int32) {
		// Every validation attempt opens source and destination. Reaching four
		// opens proves that B did not classify A's visible partial as stable.
		if count >= 4 {
			retriedOnce.Do(func() { close(retried) })
		}
	}
	b := locationTestTarget(directory, nil, false)
	b.fs = bFS
	b.retry = a.retry

	aResult := make(chan error, 1)
	go func() { aResult <- a.putBundleFile(context.Background(), file) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first Bundle publisher did not expose its partial final")
	}
	bResult := make(chan error, 1)
	go func() { bResult <- b.putBundleFile(context.Background(), file) }()
	select {
	case <-retried:
	case err := <-bResult:
		t.Fatalf("second Bundle publisher stopped without retrying: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("second Bundle publisher did not retry the in-progress final")
	}
	close(release)
	if err := <-aResult; err != nil {
		t.Fatalf("first Bundle publisher: %v", err)
	}
	if err := <-bResult; err != nil {
		t.Fatalf("second Bundle publisher: %v", err)
	}
	got, err := os.ReadFile(file.destination)
	if err != nil || !bytes.Equal(got, sourceBytes) {
		t.Fatalf("concurrent Bundle final differs from source: read=%v", err)
	}
	if opens := bFS.opens.Load(); opens < 4 || opens > 200 {
		t.Fatalf("Bundle validation opens = %d, want bounded non-busy retry", opens)
	}
}

func TestLocationBundlePreservesUnknownInvalidFinals(t *testing.T) {
	base, _ := bundleLocationFileFixture(t)
	for _, kind := range []string{"partial", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			file := base
			file.destination = filepath.Join(directory, filepath.Base(file.source))
			target := locationTestTarget(directory, nil, false)
			switch kind {
			case "partial":
				if err := os.WriteFile(file.destination, []byte("unknown partial Bundle"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, file.destination); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(file.destination, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(file.destination)
			if err != nil {
				t.Fatal(err)
			}
			err = target.putBundleFile(context.Background(), file)
			if err == nil {
				t.Fatalf("%s Bundle final was accepted", kind)
			}
			if kind == "partial" && !strings.Contains(err.Error(), "cleanup/repair is required before retry") {
				t.Fatalf("partial Bundle error = %v", err)
			}
			after, statErr := os.Lstat(file.destination)
			if statErr != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatalf("%s Bundle final changed: before=%v after=%v err=%v", kind, before, after, statErr)
			}
		})
	}
}

func TestLocationBundleOwnedPartialCleanupPreservesReplacement(t *testing.T) {
	base, _ := bundleLocationFileFixture(t)
	for _, replacement := range []bool{false, true} {
		name := "remove-owned"
		if replacement {
			name = "preserve-replacement"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			file := base
			file.destination = filepath.Join(directory, filepath.Base(file.source))
			target := locationTestTarget(directory, nil, false)
			fs := newTrackingLocationFileSystem()
			fs.wrapCreate = func(path string, created locationWriteFile) locationWriteFile {
				var once sync.Once
				return &hookedLocationWriteFile{base: created, writeHook: func([]byte) (int, error) {
					once.Do(func() {
						if replacement {
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(path, []byte("another publisher"), 0o644); err != nil {
								t.Fatal(err)
							}
						}
					})
					return 0, errInjectedLocationFailure
				}}
			}
			target.fs = fs
			err := target.putBundleFile(context.Background(), file)
			if !errors.Is(err, errInjectedLocationFailure) {
				t.Fatalf("injected Bundle write error = %v", err)
			}
			if replacement {
				got, readErr := os.ReadFile(file.destination)
				if readErr != nil || string(got) != "another publisher" {
					t.Fatalf("replacement Bundle final changed: %q err=%v", got, readErr)
				}
			} else if _, statErr := os.Lstat(file.destination); !os.IsNotExist(statErr) {
				t.Fatalf("owned partial Bundle remains: %v", statErr)
			}
		})
	}
}
