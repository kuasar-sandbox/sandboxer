package restore

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storefs "github.com/kuasar-sandbox/accelerator/pkg/store/fs"
	"github.com/kuasar-sandbox/accelerator/pkg/store/pb"
	storeserver "github.com/kuasar-sandbox/accelerator/pkg/store/server"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"google.golang.org/grpc"
)

func TestPublishManifestBundleToLocationCopiesExactlyOneFile(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	key := [32]byte{0x81, 0x82, 0x83}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(context.Background(), sourceDir, "publish-bundle", cfg,
		func() ([32]byte, error) { return key, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootRef, sourcePath, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x55}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := PublishLocalToLocation(context.Background(), filepath.Join(sourceDir, "publish-bundle.snapshot"), "snapshots", destinationDir, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	base := manifest.HexKey(root) + ".bundle"
	want := "file://" + base + "@manifest:" + manifest.HexKey(root) + "@location:snapshots"
	if got != want {
		t.Fatalf("published ref = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(destinationDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != base {
		t.Fatalf("destination entries = %v, want only %s", entries, base)
	}
	sourceBytes, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	destinationPath := filepath.Join(destinationDir, base)
	destinationBytes, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sourceBytes, destinationBytes) {
		t.Fatal("published Bundle bytes changed")
	}
	reader, err := manifestbundle.Open(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if !reader.HasManifest(root) {
		t.Fatal("published Bundle omitted root Manifest")
	}
}

func TestPublishManifestBundleToLocationCopiesOnlySameDirectoryRefs(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	key := [32]byte{0x81, 0x91, 0xa1}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	parentRef, parentPath, _ := writeUploadBundleFixture(t, sourceDir, "publish-parent", cfg, key)
	parentRoot, err := manifest.ParseKeyRef(parentRef)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := cfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	localRef := "file://" + filepath.Base(parentPath)
	locatedRef := "file://external.bundle@location:external"
	child, err := snapshot.NewPlannedBundleSink(sourceDir, "publish-child", cfg,
		func() ([32]byte, error) { return key, nil }, admission, []string{localRef, locatedRef}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {},
		"snapshot.cfg": []byte("from_refs:\n  - manifest://" + manifest.HexKey(parentRoot) + "\nboot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	childRef, childPath, err := child.AbsorbBundle(context.Background(),
		bytes.NewReader(bytes.Repeat([]byte{0x72}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	childRoot, err := manifest.ParseKeyRef(childRef)
	if err != nil {
		t.Fatal(err)
	}

	got, err := PublishLocalToLocation(context.Background(), childPath, "published", destinationDir, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "file://" + filepath.Base(childPath) + "@manifest:" + manifest.HexKey(childRoot) + "@location:published"
	if got != want {
		t.Fatalf("published ref = %q, want %q", got, want)
	}
	for _, path := range []string{parentPath, childPath} {
		sourceBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		destinationBytes, err := os.ReadFile(filepath.Join(destinationDir, filepath.Base(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(sourceBytes, destinationBytes) {
			t.Fatalf("published %s bytes changed", filepath.Base(path))
		}
	}
	if _, err := os.Stat(filepath.Join(destinationDir, "external.bundle")); !os.IsNotExist(err) {
		t.Fatalf("located external Bundle was copied: %v", err)
	}
	if reused, err := PublishLocalToLocation(context.Background(), childPath, "published", destinationDir, nil, false, nil); err != nil || reused != want {
		t.Fatalf("byte-identical Bundle reuse = %q, %v", reused, err)
	}
}

func TestPublishManifestBundleToLocationPreflightsSiblingCollision(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	key := [32]byte{0x82, 0x92, 0xa2}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	_, parentPath, _ := writeUploadBundleFixture(t, sourceDir, "collision-parent", cfg, key)
	admission, err := cfg.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	child, err := snapshot.NewPlannedBundleSink(sourceDir, "collision-child", cfg,
		func() ([32]byte, error) { return key, nil }, admission,
		[]string{"file://" + filepath.Base(parentPath)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, childPath, err := child.AbsorbBundle(context.Background(),
		bytes.NewReader(bytes.Repeat([]byte{0x73}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destinationDir, filepath.Base(parentPath)), []byte("different"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLocalToLocation(context.Background(), childPath, "published", destinationDir, nil, false, nil); err == nil {
		t.Fatal("different sibling collision was accepted")
	}
	if _, err := os.Stat(filepath.Join(destinationDir, filepath.Base(childPath))); !os.IsNotExist(err) {
		t.Fatalf("root was published after sibling preflight failed: %v", err)
	}
}

func TestPublishManifestBundleToLocationRejectsExistingSymlink(t *testing.T) {
	sourceDir := t.TempDir()
	destinationDir := t.TempDir()
	key := [32]byte{0x84, 0x85, 0x86}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(context.Background(), sourceDir, "publish-symlink", cfg,
		func() ([32]byte, error) { return key, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootRef, sourcePath, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x57}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(destinationDir, manifest.HexKey(root)+".bundle")
	if err := os.Symlink(sourcePath, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishLocalToLocation(context.Background(), sourcePath, "snapshots", destinationDir, nil, false, nil); err == nil {
		t.Fatal("existing symlink destination was accepted")
	}
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("publisher replaced the existing symlink")
	}
}

func TestUploadManifestBundleCopiesExactObjectsAndRootKey(t *testing.T) {
	sourceDir := t.TempDir()
	customerKey := [32]byte{0x91, 0x92, 0x93}
	bundleCfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(context.Background(), sourceDir, "exact-upload", bundleCfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	diskRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x26}, 8192)), nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {},
		"snapshot.cfg": []byte("boot:\n  root:\n    base: " + diskRef + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootRef, sourcePath, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x37}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	backendRoot := t.TempDir()
	backend, err := storefs.New(storefs.Config{Root: backendRoot})
	if err != nil {
		t.Fatal(err)
	}
	server, err := storeserver.New(storeserver.Options{
		Backend: backend,
		Generations: func() []store.Generation {
			return []store.Generation{"NONE"}
		},
		VerifyKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "store.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterStoreServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	uploadCfg := *bundleCfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket, Pool: 2}

	got, err := UploadLocal(context.Background(), filepath.Join(sourceDir, "exact-upload.snapshot"), &uploadCfg,
		func() ([32]byte, error) { return customerKey, nil }, nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != rootRef {
		t.Fatalf("uploaded root = %q, want unchanged %q", got, rootRef)
	}
	reader, err := manifestbundle.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, item := range []struct {
		partition store.Partition
		keys      []store.ContentKey
	}{
		{store.PartitionChunk, reader.ChunkKeys()},
		{store.PartitionManifest, reader.ManifestKeys()},
	} {
		for _, key := range item.keys {
			result, blob, err := reader.Getter().Get(context.Background(), item.partition, key)
			if err != nil || blob == nil {
				t.Fatalf("read source %s %s: result=%v err=%v", item.partition, manifest.HexKey(key), result, err)
			}
			found, stored, err := backend.Get(context.Background(), "NONE", item.partition, key)
			if err != nil || !found {
				blob.Release()
				t.Fatalf("target %s %s: found=%v err=%v", item.partition, manifest.HexKey(key), found, err)
			}
			if !bytes.Equal(stored, blob.Bytes()) {
				blob.Release()
				t.Fatalf("target rewrote %s %s", item.partition, manifest.HexKey(key))
			}
			blob.Release()
		}
	}
}

func TestUploadManifestBundleRejectsRemovedGenerationBeforePut(t *testing.T) {
	customerKey := [32]byte{0xa1, 0xa2, 0xa3}
	bundleCfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "REMOVED"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	_, path, root := writeUploadBundleFixture(t, t.TempDir(), "removed-generation", bundleCfg, customerKey)
	socket, backend := startBundleUploadStore(t, []store.Generation{"CURRENT"})
	uploadCfg := *bundleCfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket}

	_, err := UploadLocal(context.Background(), path, &uploadCfg,
		func() ([32]byte, error) { return customerKey, nil }, nil, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("removed generation upload error = %v", err)
	}
	found, _, getErr := backend.Get(context.Background(), "REMOVED", store.PartitionManifest, root)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if found {
		t.Fatal("root Manifest was published after target rejected the recorded generation")
	}
}

func TestUploadManifestBundleRejectsObjectsOutsideRecordedSaltDomain(t *testing.T) {
	customerKey := [32]byte{0xb1, 0xb2, 0xb3}
	sourceCfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	_, sourcePath, root := writeUploadBundleFixture(t, t.TempDir(), "source-domain", sourceCfg, customerKey)
	forgedDir := t.TempDir()
	forgedPath := filepath.Join(forgedDir, manifest.HexKey(root)+".bundle")
	salt, err := store.SaltForGeneration("G2")
	if err != nil {
		t.Fatal(err)
	}
	copyBundleObjectsWithAdmission(t, sourcePath, forgedPath, root,
		store.WriteAdmission{Generation: "G2", Salt: salt})

	socket, backend := startBundleUploadStore(t, []store.Generation{"G2"})
	uploadCfg := *sourceCfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket}
	_, err = UploadLocal(context.Background(), forgedPath, &uploadCfg,
		func() ([32]byte, error) { return customerKey, nil }, nil, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "salt domain") {
		t.Fatalf("forged salt-domain upload error = %v", err)
	}
	found, _, getErr := backend.Get(context.Background(), "G2", store.PartitionManifest, root)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if found {
		t.Fatal("root Manifest was published from a Bundle with forged admission provenance")
	}
}

func TestUploadManifestBundleUsesOrderedSourcesAndEachAdmission(t *testing.T) {
	fixture := writeMultiSourceUploadFixture(t)
	socket, backend := startBundleUploadStore(t, []store.Generation{"G1", "G2", "G3"})
	uploadCfg := *fixture.currentCfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket, Pool: 3}

	got, err := UploadLocalWithLocations(context.Background(), fixture.currentPath, &uploadCfg,
		func() ([32]byte, error) { return fixture.customerKey, nil }, nil, fixture.locations, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "manifest://" + manifest.HexKey(fixture.currentRoot); got != want {
		t.Fatalf("uploaded root = %q, want %q", got, want)
	}
	for _, source := range []struct {
		path       string
		generation store.Generation
	}{
		{fixture.sourceAPath, "G1"},
		{fixture.sourceBPath, "G2"},
		{fixture.currentPath, "G3"},
	} {
		reader, err := manifestbundle.Open(source.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range []struct {
			partition store.Partition
			keys      []store.ContentKey
		}{
			{store.PartitionChunk, reader.ChunkKeys()},
			{store.PartitionManifest, reader.ManifestKeys()},
		} {
			for _, key := range item.keys {
				result, blob, err := reader.Getter().Get(context.Background(), item.partition, key)
				if err != nil || blob == nil {
					_ = reader.Close()
					t.Fatalf("read source %s %s: result=%v err=%v", item.partition, manifest.HexKey(key), result, err)
				}
				found, stored, err := backend.Get(context.Background(), source.generation, item.partition, key)
				if err != nil || !found {
					blob.Release()
					_ = reader.Close()
					t.Fatalf("target %s/%s %s: found=%v err=%v", source.generation, item.partition, manifest.HexKey(key), found, err)
				}
				if !bytes.Equal(stored, blob.Bytes()) {
					blob.Release()
					_ = reader.Close()
					t.Fatalf("target rewrote %s/%s %s", source.generation, item.partition, manifest.HexKey(key))
				}
				blob.Release()
			}
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUploadManifestBundlePreflightsAllSourceAdmissionsBeforePut(t *testing.T) {
	fixture := writeMultiSourceUploadFixture(t)
	socket, backend := startBundleUploadStore(t, []store.Generation{"G1", "G3"})
	uploadCfg := *fixture.currentCfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket}

	_, err := UploadLocalWithLocations(context.Background(), fixture.currentPath, &uploadCfg,
		func() ([32]byte, error) { return fixture.customerKey, nil }, nil, fixture.locations, nil, false, nil)
	if err == nil || !strings.Contains(err.Error(), "G2") {
		t.Fatalf("removed dependency generation error = %v", err)
	}
	for _, item := range []struct {
		generation store.Generation
		key        store.ContentKey
	}{
		{"G1", fixture.sourceARoot},
		{"G3", fixture.currentRoot},
	} {
		found, _, getErr := backend.Get(context.Background(), item.generation, store.PartitionManifest, item.key)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if found {
			t.Fatalf("Manifest %s was published before all admissions passed", manifest.HexKey(item.key))
		}
	}
}

func TestUploadManifestBundleReusesStrictStoreDependencyAfterBundleMiss(t *testing.T) {
	fixture := writeMultiSourceUploadFixture(t)
	socket, backend := startBundleUploadStore(t, []store.Generation{"G1", "G2", "G3"})
	uploadCfg := *fixture.currentCfg
	uploadCfg.Store = manifest.StoreConfig{Endpoint: socket, Pool: 2}

	// Seed B through its own exact path, then make the listed Bundle unavailable.
	if _, err := UploadLocal(context.Background(), fixture.sourceBPath, &uploadCfg,
		func() ([32]byte, error) { return fixture.customerKey, nil }, nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.sourceBPath); err != nil {
		t.Fatal(err)
	}
	got, err := UploadLocalWithLocations(context.Background(), fixture.currentPath, &uploadCfg,
		func() ([32]byte, error) { return fixture.customerKey, nil }, nil, fixture.locations, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "manifest://" + manifest.HexKey(fixture.currentRoot); got != want {
		t.Fatalf("uploaded root = %q, want %q", got, want)
	}
	found, _, err := backend.Get(context.Background(), "G3", store.PartitionManifest, fixture.currentRoot)
	if err != nil || !found {
		t.Fatalf("root after Store dependency reuse: found=%v err=%v", found, err)
	}
}

type multiSourceUploadFixture struct {
	customerKey                           [32]byte
	currentCfg                            *manifest.Config
	sourceAPath, sourceBPath, currentPath string
	sourceARoot, sourceBRoot, currentRoot store.ContentKey
	locations                             config.RefLocations
}

func writeMultiSourceUploadFixture(t testing.TB) multiSourceUploadFixture {
	t.Helper()
	customerKey := [32]byte{0xc1, 0xc2, 0xc3}
	configFor := func(generation string) *manifest.Config {
		return &manifest.Config{
			Manifest: manifest.ManifestSubConfig{WriteGeneration: generation},
			Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
			Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
		}
	}
	dirA, dirB := t.TempDir(), t.TempDir()
	cfgA, cfgB, cfgC := configFor("G1"), configFor("G2"), configFor("G3")
	refA, pathA, rootA := writeUploadBundleFixture(t, dirA, "source-a", cfgA, customerKey)
	refB, pathB, rootB := writeUploadBundleFixture(t, dirB, "source-b", cfgB, customerKey)
	if refA != "manifest://"+manifest.HexKey(rootA) || refB != "manifest://"+manifest.HexKey(rootB) {
		t.Fatal("source fixture returned inconsistent root ref")
	}
	refs := []string{
		"file://" + filepath.Base(pathA) + "@location:A",
		"file://" + filepath.Base(pathB) + "@location:B",
	}
	admission, err := cfgC.WriteAdmission(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	currentDir := t.TempDir()
	sink, err := snapshot.NewPlannedBundleSink(currentDir, "source-current", cfgC,
		func() ([32]byte, error) { return customerKey, nil }, admission, refs, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {},
		"snapshot.cfg": []byte("from_refs:\n  - manifest://" + manifest.HexKey(rootA) +
			"\n  - manifest://" + manifest.HexKey(rootB) +
			"\nboot:\n  root:\n    base: manifest://" + manifest.HexKey(rootB) + "\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	currentRef, currentPath, err := sink.AbsorbBundle(context.Background(),
		bytes.NewReader(bytes.Repeat([]byte{0x7c}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	currentRoot, err := manifest.ParseKeyRef(currentRef)
	if err != nil {
		t.Fatal(err)
	}
	return multiSourceUploadFixture{
		customerKey: customerKey, currentCfg: cfgC,
		sourceAPath: pathA, sourceBPath: pathB, currentPath: currentPath,
		sourceARoot: rootA, sourceBRoot: rootB, currentRoot: currentRoot,
		locations: config.RefLocations{"A": dirA, "B": dirB},
	}
}

func writeUploadBundleFixture(t testing.TB, dir, sid string, cfg *manifest.Config, customerKey [32]byte) (string, string, store.ContentKey) {
	t.Helper()
	sink, err := snapshot.NewBundleSink(context.Background(), dir, sid, cfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	inner, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootRef, path, err := sink.AbsorbBundle(context.Background(),
		bytes.NewReader(bytes.Repeat([]byte{0x65}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	return rootRef, path, root
}

func copyBundleObjectsWithAdmission(t testing.TB, sourcePath, destinationPath string, root store.ContentKey, admission store.WriteAdmission) {
	t.Helper()
	reader, err := manifestbundle.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := manifestbundle.NewWriter(destination, admission, manifestbundle.WriterOptions{})
	if err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	for _, item := range []struct {
		partition store.Partition
		keys      []store.ContentKey
	}{
		{store.PartitionChunk, reader.ChunkKeys()},
		{store.PartitionManifest, reader.ManifestKeys()},
	} {
		for _, key := range item.keys {
			_, blob, err := reader.Getter().Get(context.Background(), item.partition, key)
			if err != nil || blob == nil {
				_ = destination.Close()
				t.Fatalf("read source %s %s: %v", item.partition, manifest.HexKey(key), err)
			}
			_, putErr := writer.Put(context.Background(), admission, item.partition, key, blob.Bytes())
			blob.Release()
			if putErr != nil {
				_ = destination.Close()
				t.Fatal(putErr)
			}
		}
	}
	if err := writer.Finalize(root); err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	if err := destination.Sync(); err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
}

func startBundleUploadStore(t testing.TB, generations []store.Generation) (string, *storefs.Store) {
	t.Helper()
	backend, err := storefs.New(storefs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	server, err := storeserver.New(storeserver.Options{
		Backend: backend,
		Generations: func() []store.Generation {
			return append([]store.Generation(nil), generations...)
		},
		VerifyKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "store.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterStoreServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})
	return socket, backend
}
