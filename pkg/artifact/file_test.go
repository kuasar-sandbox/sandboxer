package artifact_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func artifactSnapshotSource(t testing.TB, memory, snapshotConfig []byte) sparse.Source {
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

func absorbBundleSnapshot(t testing.TB, sink *snapshot.BundleSink, directory string, memory, snapshotConfig []byte) (string, string) {
	t.Helper()
	ref, _, err := sink.AbsorbSnapshot(context.Background(), artifactSnapshotSource(t, memory, snapshotConfig))
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

func TestOpenFileManifestBundleSelectorAndScopedFetcher(t *testing.T) {
	dir := t.TempDir()
	customerKey := [32]byte{0x71, 0x72, 0x73}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(context.Background(), dir, "artifact-open", cfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	overlayRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x21}, 8192)), nil)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, bundlePath := absorbBundleSnapshot(t, sink, dir, bytes.Repeat([]byte{0x31}, 8192),
		[]byte("version: 1\nsandbox_ref: "+overlayRef+"\n"))
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MANIFEST_KEY", hex.EncodeToString(customerKey[:]))
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	symlink := filepath.Join(dir, "artifact-open.snapshot")
	opened, err := storage.OpenFile(context.Background(), symlink,
		manifest.Ref{Scheme: manifest.RefSchemeFile, Path: symlink})
	if err != nil {
		t.Fatal(err)
	}
	if opened.Format() != artifact.FileFormatManifestBundle {
		t.Fatalf("format = %v, want Manifest Bundle", opened.Format())
	}
	rootKey, ok := opened.RootManifestKey()
	if !ok || rootRef != "manifest://"+manifest.HexKey(rootKey) {
		t.Fatalf("selected root = %s, want %s", manifest.HexKey(rootKey), rootRef)
	}
	overlayKey, err := manifest.ParseKeyRef(overlayRef)
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := opened.ScopedFetcher().OpenManifest(context.Background(), overlayKey)
	if err != nil {
		t.Fatal(err)
	}
	if overlay.Size() != 8192 {
		t.Fatalf("overlay size = %d", overlay.Size())
	}
	if err := overlay.Close(); err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	wrong := strings.Repeat("a", 64)
	ref, err := manifest.ParseRef("file://" + bundlePath + "@manifest:" + wrong)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.OpenFile(context.Background(), bundlePath, ref); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("missing explicit root error = %v", err)
	}
}

func TestOpenFileZIPMagicFailsClosedAndTarRejectsManifestSelector(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "bad.snapshot")
	if err := os.WriteFile(malformed, []byte{'P', 'K', 0x03, 0x04, 1, 2, 3}, 0o644); err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorage(&manifest.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.OpenFile(context.Background(), malformed,
		manifest.Ref{Scheme: manifest.RefSchemeFile, Path: malformed}); err == nil || !strings.Contains(err.Error(), "manifest bundle") {
		t.Fatalf("malformed ZIP error = %v", err)
	}

	_, _, tarPath := writeTarSnapshot(t, dir,
		[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("a", 64)+"\n"))
	selector := strings.Repeat("b", 64)
	ref, err := manifest.ParseRef("file://" + tarPath + "@manifest:" + selector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.OpenFile(context.Background(), tarPath, ref); err == nil || !strings.Contains(err.Error(), "tarstream rejects") {
		t.Fatalf("tarstream @manifest error = %v", err)
	}
}

func TestOpenFileLocatedAliasCannotEscapeLocationDirectory(t *testing.T) {
	ctx := context.Background()
	locationDir := t.TempDir()
	outsideDir := t.TempDir()
	refRaw, _, outsidePath := writeTarSnapshot(t, outsideDir,
		[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("a", 64)+"\n"))
	ref, err := manifest.ParseRef(refRaw)
	if err != nil {
		t.Fatal(err)
	}
	ref.Path = "outside.snapshot"
	ref.Location = "artifacts"
	escaping := filepath.Join(locationDir, ref.Path)
	if err := os.Symlink(outsidePath, escaping); err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	locations := config.RefLocations{"artifacts": locationDir}
	if _, err := storage.OpenFileWithLocations(ctx, escaping, ref, locations); err == nil || !strings.Contains(err.Error(), "escapes ref-location") {
		t.Fatalf("escaping located alias error = %v", err)
	}
	if _, err := storage.OpenFileWithLocations(ctx, outsidePath, ref, locations); err == nil || !strings.Contains(err.Error(), "does not match ref-location") {
		t.Fatalf("mismatched located path error = %v", err)
	}

	insideRaw, _, insidePath := writeTarSnapshot(t, locationDir,
		[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("b", 64)+"\n"))
	insideRef, err := manifest.ParseRef(insideRaw)
	if err != nil {
		t.Fatal(err)
	}
	insideRef.Path = "inside.snapshot"
	insideRef.Location = "artifacts"
	insideAlias := filepath.Join(locationDir, insideRef.Path)
	if err := os.Symlink(filepath.Base(insidePath), insideAlias); err != nil {
		t.Fatal(err)
	}
	opened, err := storage.OpenFileWithLocations(ctx, insideAlias, insideRef, locations)
	if err != nil {
		t.Fatalf("same-directory located alias: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenFileBundleHonorsVerifyContentPolicy(t *testing.T) {
	dir := t.TempDir()
	customerKey := [32]byte{0x81, 0x82, 0x83}
	keyFn := func() ([32]byte, error) { return customerKey, nil }
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := snapshot.NewBundleSink(context.Background(), dir, "verify-source", cfg, keyFn, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootRef, sourcePath := absorbBundleSnapshot(t, sink, dir, bytes.Repeat([]byte{0x45}, 8192),
		[]byte("version: 1\nsandbox_ref: manifest://"+strings.Repeat("a", 64)+"\n"))
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := manifestbundle.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	wrongRoot := store.ContentKey{0xfe, 0xed, 0xfa, 0xce}
	forgedPath := filepath.Join(dir, manifest.HexKey(wrongRoot)+".bundle")
	file, err := os.OpenFile(forgedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := manifestbundle.NewWriter(file, reader.Admission(), manifestbundle.WriterOptions{})
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	for _, chunkKey := range reader.ChunkKeys() {
		_, blob, err := reader.Getter().Get(context.Background(), store.PartitionChunk, chunkKey)
		if err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		_, putErr := writer.Put(context.Background(), reader.Admission(), store.PartitionChunk, chunkKey, blob.Bytes())
		blob.Release()
		if putErr != nil {
			_ = file.Close()
			t.Fatal(putErr)
		}
	}
	_, manifestBlob, err := reader.Getter().Get(context.Background(), store.PartitionManifest, root)
	if err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	_, putErr := writer.Put(context.Background(), reader.Admission(), store.PartitionManifest, wrongRoot, manifestBlob.Bytes())
	manifestBlob.Release()
	if putErr != nil {
		_ = file.Close()
		t.Fatal(putErr)
	}
	if err := writer.Finalize(wrongRoot); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	verify := true
	strictCfg := *cfg
	strictCfg.Manifest.VerifyContent = &verify
	ref := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: forgedPath}
	if _, err := artifact.OpenFile(context.Background(), forgedPath, ref, &strictCfg, keyFn, nil, nil, false); err == nil || !strings.Contains(err.Error(), "content key mismatch") {
		t.Fatalf("strict Bundle open error = %v", err)
	}
	verify = false
	uncheckedCfg := *cfg
	uncheckedCfg.Manifest.VerifyContent = &verify
	opened, err := artifact.OpenFile(context.Background(), forgedPath, ref, &uncheckedCfg, keyFn, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	buffer := make([]byte, 4096)
	if n, err := opened.ReadAt(context.Background(), buffer, 0); err != nil || n != len(buffer) {
		t.Fatalf("unchecked Bundle read = %d, %v", n, err)
	}
}

func writeTarSnapshot(t *testing.T, dir string, snapshotConfig []byte) (string, string, string) {
	t.Helper()
	returnValues := make([]string, 3)
	sink := snapshot.NewFileSink(dir, "tar-artifact", nil, false, nil)
	ref, path, err := sink.AbsorbSnapshot(context.Background(), artifactSnapshotSource(t,
		bytes.Repeat([]byte{0x44}, 4096), snapshotConfig))
	if err != nil {
		t.Fatal(err)
	}
	returnValues[0], returnValues[2] = ref, path
	return returnValues[0], returnValues[1], returnValues[2]
}
