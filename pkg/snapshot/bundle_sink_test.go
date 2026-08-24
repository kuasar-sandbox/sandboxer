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

	inner, err := BuildZIP(map[string][]byte{
		"config.json":  {},
		"state.json":   {},
		"snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rootRef, path, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x55}, 8192)), nil, bytes.NewReader(inner))
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := manifest.ParseKeyRef(rootRef)
	if err != nil {
		t.Fatal(err)
	}
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
	inner, err := BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": []byte("boot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sink.AbsorbBundle(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x33}, 4096)), nil, bytes.NewReader(inner)); err != nil {
		t.Fatal(err)
	}
	if got := server.calls.Load(); got != 1 {
		t.Fatalf("Store.AdmitWrite calls = %d, want exactly 1 for all Manifests", got)
	}
}
