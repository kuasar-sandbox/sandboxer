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
	want := "file://" + base + "@location:snapshots"
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
