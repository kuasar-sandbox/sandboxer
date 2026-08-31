package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestbundle "github.com/kuasar-sandbox/accelerator/pkg/manifest/bundle"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	"github.com/kuasar-sandbox/accelerator/pkg/store/pb"
	"google.golang.org/grpc"
)

func TestBundleSinkWritesOneMultiManifestFile(t *testing.T) {
	dir := t.TempDir()
	customerKey := [32]byte{0x41, 0x42, 0x43}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := NewBundleSink(context.Background(), dir, "sandbox-a", cfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	shared := bytes.Repeat([]byte{0x11}, 4096)
	first := append(append([]byte(nil), shared...), bytes.Repeat([]byte{0x22}, 4096)...)
	second := append(append([]byte(nil), shared...), bytes.Repeat([]byte{0x33}, 4096)...)
	firstRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(first), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondRef, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(second), nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstRef == secondRef {
		t.Fatal("distinct overlays produced the same Manifest ref")
	}

	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	rootRef, _, err := sink.AbsorbSnapshot(context.Background(),
		testSnapshotSource(t, bytes.Repeat([]byte{0x55}, 8192), nil, snapshotConfig))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(context.Background(), rootRef, ""); err != nil {
		t.Fatal(err)
	}
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, manifest.HexKey(rootKey)+".bundle")
	if filepath.Base(path) != manifest.HexKey(rootKey)+".bundle" {
		t.Fatalf("Bundle path = %q, root = %s", path, rootRef)
	}
	link, err := os.Readlink(filepath.Join(dir, "sandbox-a.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if link != filepath.Base(path) {
		t.Fatalf("snapshot symlink = %q, want %q", link, filepath.Base(path))
	}

	reader, err := manifestbundle.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.Admission().Generation != "NONE" {
		t.Fatalf("generation = %q, want NONE", reader.Admission().Generation)
	}
	if got := len(reader.ManifestKeys()); got != 3 {
		t.Fatalf("Manifest count = %d, want 3", got)
	}
	// Two disk layers share their first 4 KiB Chunk. Without Bundle-level
	// object dedup this would be six physical Chunks rather than five.
	if got := len(reader.ChunkKeys()); got != 5 {
		t.Fatalf("unique Chunk count = %d, want 5", got)
	}
	_, decryptor, err := manifestcrypto.New(cfg.Crypto)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.FullVerify(context.Background(), rootKey, customerKey, decryptor,
		manifestbundle.VerifyOptions{ExpectedManifests: reader.ManifestKeys()}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleSinkRepairsInconsistentExistingFinal(t *testing.T) {
	dir := t.TempDir()
	customerKey := [32]byte{0x44, 0x45, 0x46}
	cfg := &manifest.Config{
		Chunker: chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:  manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	payload := bytes.Repeat([]byte{0x61}, 8192)
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	write := func(sandboxID string) (store.ContentKey, string) {
		sink, err := NewBundleSink(context.Background(), dir, sandboxID, cfg,
			func() ([32]byte, error) { return customerKey, nil }, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sink.Close() })
		rootRef, _, err := sink.AbsorbSnapshot(context.Background(),
			testSnapshotSource(t, payload, nil, snapshotConfig))
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.CommitSnapshot(context.Background(), rootRef, ""); err != nil {
			t.Fatal(err)
		}
		root, err := manifest.ParseKeyRef(rootRef)
		if err != nil {
			t.Fatal(err)
		}
		return root, filepath.Join(dir, manifest.HexKey(root)+".bundle")
	}

	root, path := write("first")
	if err := os.WriteFile(path, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	repairedRoot, repairedPath := write("second")
	if repairedRoot != root || repairedPath != path {
		t.Fatalf("repaired root/path = %s %q, want %s %q", manifest.HexKey(repairedRoot), repairedPath, manifest.HexKey(root), path)
	}
	reader, err := manifestbundle.Open(path)
	if err != nil {
		t.Fatalf("open repaired Bundle: %v", err)
	}
	defer reader.Close()
	_, decryptor, err := manifestcrypto.New(cfg.Crypto)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.FullVerify(context.Background(), root, customerKey, decryptor,
		manifestbundle.VerifyOptions{ExpectedManifests: reader.ManifestKeys()}); err != nil {
		t.Fatalf("verify repaired Bundle: %v", err)
	}
}

type admissionOnlyStore struct {
	pb.UnimplementedStoreServer
	calls      atomic.Int32
	generation store.Generation
}

func (s *admissionOnlyStore) AdmitWrite(_ context.Context, request *pb.AdmitWriteRequest) (*pb.AdmitWriteResponse, error) {
	s.calls.Add(1)
	if request.GetGeneration() != string(s.generation) {
		return nil, fmt.Errorf("generation = %q, want %q", request.GetGeneration(), s.generation)
	}
	salt, err := store.SaltForGeneration(s.generation)
	if err != nil {
		return nil, err
	}
	return &pb.AdmitWriteResponse{Generation: string(s.generation), Salt: salt[:]}, nil
}

func TestBundleSinkAcquiresOnlineAdmissionOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(t.TempDir(), "store.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &admissionOnlyStore{generation: "G1"}
	grpcServer := grpc.NewServer()
	pb.RegisterStoreServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	customerKey := [32]byte{0x51, 0x52, 0x53}
	cfg := &manifest.Config{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Store:    manifest.StoreConfig{Endpoint: socket},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes"},
	}
	sink, err := NewBundleSink(context.Background(), dir, "one-admission", cfg,
		func() ([32]byte, error) { return customerKey, nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	for _, value := range []byte{0x11, 0x22} {
		if _, _, err := sink.AbsorbOverlay(context.Background(), bytes.NewReader(bytes.Repeat([]byte{value}, 4096)), nil); err != nil {
			t.Fatal(err)
		}
	}
	rootRef, _, err := sink.AbsorbSnapshot(context.Background(), testSnapshotSource(t,
		bytes.Repeat([]byte{0x33}, 4096), nil,
		[]byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.CommitSnapshot(context.Background(), rootRef, ""); err != nil {
		t.Fatal(err)
	}
	if got := server.calls.Load(); got != 1 {
		t.Fatalf("Store.AdmitWrite calls = %d, want exactly 1 for all Manifests", got)
	}
}
