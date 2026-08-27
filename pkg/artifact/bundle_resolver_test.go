package artifact_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestBundleResolverOrderedLazyFlatAndFailClosed(t *testing.T) {
	customerKey := [32]byte{0x41, 0x42, 0x43}
	manifestCfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	locationA := t.TempDir()
	locationB := t.TempDir()
	_, sourceA, targetKey := writeResolverBundle(t, locationA, "source-a", manifestCfg, keyFn, nil, true, 0x51)
	_, sourceB, targetKeyB := writeResolverBundle(t, locationB, "source-b", manifestCfg, keyFn, nil, true, 0x51)
	if targetKey != targetKeyB {
		t.Fatal("identical source layers produced different Manifest keys")
	}
	base := filepath.Base(sourceA)
	if filepath.Base(sourceB) != base {
		t.Fatal("identical source Bundles produced different root names")
	}
	refs := []string{
		"file://" + base + "@location:A",
		"file://" + base + "@location:B",
	}
	locations := config.RefLocations{"A": locationA, "B": locationB}

	t.Run("first hit and cached reader", func(t *testing.T) {
		_, currentPath, _ := writeResolverBundle(t, t.TempDir(), "current", manifestCfg, keyFn, refs, false, 0x61)
		opened := openResolverRoot(t, currentPath, manifestCfg, keyFn, locations)
		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				stream, err := opened.ManifestFetcher().OpenManifest(context.Background(), targetKey)
				if err != nil {
					errs <- err
					return
				}
				buffer := make([]byte, 4096)
				n, readErr := stream.ReadAt(context.Background(), buffer, 0)
				closeErr := stream.Close()
				if readErr != nil || closeErr != nil || n != len(buffer) || !bytes.Equal(buffer, bytes.Repeat([]byte{0x51}, len(buffer))) {
					errs <- fmt.Errorf("concurrent selected read = %d, read=%v close=%v", n, readErr, closeErr)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		source, err := opened.ManifestFetcher().SelectManifest(context.Background(), targetKey)
		if err != nil {
			t.Fatal(err)
		}
		if source.Ref != refs[0] {
			t.Fatalf("selected source = %q, want first ref %q", source.Ref, refs[0])
		}
		if err := os.Remove(sourceA); err != nil {
			t.Fatal(err)
		}
		again, err := opened.ManifestFetcher().SelectManifest(context.Background(), targetKey)
		if err != nil || again.Reader != source.Reader {
			t.Fatalf("cached source = %#v, %v", again, err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := source.OpenManifest(context.Background(), targetKey); err == nil {
			t.Fatal("referenced Reader remained usable after root close")
		}
	})

	t.Run("missing source continues", func(t *testing.T) {
		missingA := t.TempDir()
		_, currentPath, _ := writeResolverBundle(t, t.TempDir(), "current-missing", manifestCfg, keyFn, refs, false, 0x62)
		opened := openResolverRoot(t, currentPath, manifestCfg, keyFn,
			config.RefLocations{"A": missingA, "B": locationB})
		defer opened.Close()
		source, err := opened.ManifestFetcher().SelectManifest(context.Background(), targetKey)
		if err != nil {
			t.Fatal(err)
		}
		if source.Ref != refs[1] {
			t.Fatalf("selected source = %q, want second ref %q", source.Ref, refs[1])
		}
	})

	t.Run("malformed existing source fails closed", func(t *testing.T) {
		malformedA := t.TempDir()
		if err := os.WriteFile(filepath.Join(malformedA, base), []byte{'P', 'K', 0x03, 0x04, 1}, 0o644); err != nil {
			t.Fatal(err)
		}
		_, currentPath, _ := writeResolverBundle(t, t.TempDir(), "current-malformed", manifestCfg, keyFn, refs, false, 0x63)
		opened := openResolverRoot(t, currentPath, manifestCfg, keyFn,
			config.RefLocations{"A": malformedA, "B": locationB})
		defer opened.Close()
		_, err := opened.ManifestFetcher().SelectManifest(context.Background(), targetKey)
		if err == nil || !strings.Contains(err.Error(), refs[0]) {
			t.Fatalf("malformed first source error = %v", err)
		}
	})

	t.Run("referenced refs are not recursive", func(t *testing.T) {
		leafDir := t.TempDir()
		_, leafPath, leafKey := writeResolverBundle(t, leafDir, "leaf", manifestCfg, keyFn, nil, true, 0x71)
		middleDir := t.TempDir()
		leafRef := "file://" + filepath.Base(leafPath) + "@location:leaf"
		_, middlePath, _ := writeResolverBundle(t, middleDir, "middle", manifestCfg, keyFn, []string{leafRef}, false, 0x72)
		middleRef := "file://" + filepath.Base(middlePath) + "@location:middle"
		_, currentPath, _ := writeResolverBundle(t, t.TempDir(), "flat-root", manifestCfg, keyFn, []string{middleRef}, false, 0x73)
		opened := openResolverRoot(t, currentPath, manifestCfg, keyFn,
			config.RefLocations{"middle": middleDir, "leaf": leafDir})
		defer opened.Close()
		if _, err := opened.ManifestFetcher().SelectManifest(context.Background(), leafKey); err == nil {
			t.Fatal("referenced Bundle's own refs participated recursively")
		}
	})

	t.Run("same-directory ref follows final root symlink", func(t *testing.T) {
		physicalDir := t.TempDir()
		_, sourcePath, sourceKey := writeResolverBundle(t, physicalDir, "symlink-source", manifestCfg, keyFn, nil, true, 0x74)
		localRef := "file://" + filepath.Base(sourcePath)
		_, currentPath, _ := writeResolverBundle(t, physicalDir, "symlink-root", manifestCfg, keyFn, []string{localRef}, false, 0x75)
		aliasDir := t.TempDir()
		aliasPath := filepath.Join(aliasDir, "root.snapshot")
		if err := os.Symlink(currentPath, aliasPath); err != nil {
			t.Fatal(err)
		}
		opened := openResolverRoot(t, aliasPath, manifestCfg, keyFn, nil)
		defer opened.Close()
		source, err := opened.ManifestFetcher().SelectManifest(context.Background(), sourceKey)
		if err != nil {
			t.Fatal(err)
		}
		if source.Ref != localRef {
			t.Fatalf("selected source = %q, want %q", source.Ref, localRef)
		}
	})
}

func TestBundleResolverClosesEveryReferencedResource(t *testing.T) {
	if _, err := os.Stat("/proc/self/maps"); err != nil {
		t.Skip("/proc resource accounting is unavailable")
	}
	customerKey := [32]byte{0x44, 0x45, 0x46}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	const refCount = 32
	refs := make([]string, 0, refCount)
	keys := make([]store.ContentKey, 0, refCount)
	locations := make(config.RefLocations, refCount)
	directories := make([]string, 0, refCount+1)
	for index := range refCount {
		directory := t.TempDir()
		name := fmt.Sprintf("source-%02d", index)
		_, path, key := writeResolverBundle(t, directory, name, cfg, keyFn, nil, true, byte(0x20+index))
		refs = append(refs, "file://"+filepath.Base(path)+"@location:"+name)
		keys = append(keys, key)
		locations[name] = directory
		directories = append(directories, directory)
	}
	rootDir := t.TempDir()
	_, rootPath, _ := writeResolverBundle(t, rootDir, "resource-root", cfg, keyFn, refs, false, 0x70)
	directories = append(directories, rootDir)

	beforeMaps, beforeFDs := countBundleResources(t, directories)
	if beforeMaps != 0 || beforeFDs != 0 {
		t.Fatalf("fixture retained resources before open: mmap=%d fd=%d", beforeMaps, beforeFDs)
	}
	opened := openResolverRoot(t, rootPath, cfg, keyFn, locations)
	for _, key := range keys {
		if _, err := opened.ManifestFetcher().SelectManifest(context.Background(), key); err != nil {
			_ = opened.Close()
			t.Fatal(err)
		}
	}
	openMaps, openFDs := countBundleResources(t, directories)
	if openMaps+openFDs < refCount+1 {
		_ = opened.Close()
		t.Fatalf("open resources = mmap %d + fd %d, want at least %d", openMaps, openFDs, refCount+1)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	closedMaps, closedFDs := countBundleResources(t, directories)
	if closedMaps != 0 || closedFDs != 0 {
		t.Fatalf("Bundle resources after close: mmap=%d fd=%d", closedMaps, closedFDs)
	}
	t.Logf("32 refs resources: open mmap=%d fd=%d; after Close mmap=%d fd=%d", openMaps, openFDs, closedMaps, closedFDs)
}

func countBundleResources(t testing.TB, directories []string) (mappings, descriptors int) {
	t.Helper()
	matches := func(path string) bool {
		if !strings.Contains(path, ".bundle") {
			return false
		}
		for _, directory := range directories {
			if strings.Contains(path, filepath.Clean(directory)+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(maps), "\n") {
		if matches(line) {
			mappings++
		}
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && matches(target) {
			descriptors++
		}
	}
	return mappings, descriptors
}

func writeResolverBundle(t testing.TB, directory, sid string, cfg *manifest.Config, keyFn func() ([32]byte, error), refs []string, withOverlay bool, fill byte) (store.ContentKey, string, store.ContentKey) {
	t.Helper()
	admission, err := cfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sink, err := snapshot.NewPlannedBundleSink(directory, sid, cfg, keyFn, admission, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	var layerKey store.ContentKey
	if withOverlay {
		layerRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(bytes.Repeat([]byte{fill}, 8192)), nil)
		if err != nil {
			t.Fatal(err)
		}
		layerKey, err = manifest.ParseKeyRef(layerRef)
		if err != nil {
			t.Fatal(err)
		}
	}
	rootRef, path := absorbBundleSnapshot(t, sink, directory,
		bytes.Repeat([]byte{fill + 1}, 8192),
		[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("a", 64)+"\n"))
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	return root, path, layerKey
}

func openResolverRoot(t testing.TB, path string, cfg *manifest.Config, keyFn func() ([32]byte, error), locations config.RefLocations) *artifact.OpenedFile {
	t.Helper()
	opened, err := artifact.OpenFileWithLocations(context.Background(), path,
		manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path}, cfg, keyFn, nil, locations, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Format() != artifact.FileFormatManifestBundle || opened.ManifestFetcher() == nil {
		_ = opened.Close()
		t.Fatal("opened artifact is not a Manifest Bundle")
	}
	return opened
}
