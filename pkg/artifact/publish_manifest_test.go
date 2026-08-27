package artifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storefs "github.com/kuasar-sandbox/accelerator/pkg/store/fs"
	"github.com/kuasar-sandbox/accelerator/pkg/store/pb"
	storeserver "github.com/kuasar-sandbox/accelerator/pkg/store/server"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func manifestPublisherFixture(t *testing.T) (*config.ManifestConfig, *ProcessStorage) {
	t.Helper()
	backend, err := storefs.New(storefs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	server, err := storeserver.New(storeserver.Options{
		Backend: backend,
		Generations: func() []store.Generation {
			return []store.Generation{"G1"}
		},
		VerifyKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := t.TempDir() + "/store.sock"
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

	key := [32]byte{0x11, 0x22, 0x33}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(key[:]))
	cfg := &config.ManifestConfig{
		Manifest: manifest.ManifestSubConfig{WriteGeneration: "G1"},
		Store:    manifest.StoreConfig{Endpoint: socket, Pool: 2, Timeout: "5s"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: manifestcrypto.LocalOff},
	}
	storage, err := NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return cfg, storage
}

func TestManifestPublisherPublishesAndReopensSandboxAndSnapshot(t *testing.T) {
	ctx := context.Background()
	cfg, storage := manifestPublisherFixture(t)
	inputDir := t.TempDir()
	sink := snapshot.NewFileSink(inputDir, "manifest", nil, false, nil)
	runtimeBytes, _ := publishPortable(t, "")
	eLogical, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x41}, 8192)), nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, ePath, err := sink.AbsorbSandbox(ctx, eLogical)
	if err != nil {
		t.Fatal(err)
	}
	ePublisher, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	eResult, publishErr := ePublisher.Publish(ctx, ePath)
	closeErr := ePublisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish E err=%v close=%v", publishErr, closeErr)
	}
	if eResult.Role != RoleSandbox {
		t.Fatalf("E role = %q", eResult.Role)
	}
	if info, err := storage.Inspect(ctx, eResult.Ref, nil); err != nil || info.Role != RoleSandbox {
		t.Fatalf("inspect E = %+v err=%v", info, err)
	}

	sCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: eResult.Ref,
	})
	if err != nil {
		t.Fatal(err)
	}
	sLogical, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x52}, 8192)), []byte("{}"), []byte("{}"), sCfg)
	if err != nil {
		t.Fatal(err)
	}
	_, sPath, err := sink.AbsorbSnapshot(ctx, sLogical)
	if err != nil {
		t.Fatal(err)
	}
	sPublisher, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sResult, publishErr := sPublisher.Publish(ctx, sPath)
	closeErr = sPublisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish S err=%v close=%v", publishErr, closeErr)
	}
	if sResult.Role != RoleSnapshot {
		t.Fatalf("S role = %q", sResult.Role)
	}
	info, err := storage.Inspect(ctx, sResult.Ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Role != RoleSnapshot || info.Snapshot.SandboxRef == "" {
		t.Fatalf("inspect S = %+v", info)
	}
}
