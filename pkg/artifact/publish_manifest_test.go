package artifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

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

type blockingPublishStoreWriter struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (w *blockingPublishStoreWriter) AdmitWrite(context.Context) (store.WriteAdmission, error) {
	return store.WriteAdmission{Generation: "G1"}, nil
}

func (*blockingPublishStoreWriter) PoolSize() int { return 2 }

func (w *blockingPublishStoreWriter) Put(ctx context.Context, _ store.WriteAdmission, _ store.Partition, _ store.ContentKey, _ []byte) (bool, error) {
	if w.calls.Add(1) != 1 {
		return false, errors.New("same key reached the underlying writer twice")
	}
	close(w.started)
	select {
	case <-w.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestManifestPublisherCoalescesConcurrentSameKeyPuts(t *testing.T) {
	inner := &blockingPublishStoreWriter{started: make(chan struct{}), release: make(chan struct{})}
	writer := newDeduplicatingStoreWriter(inner)
	waiting := make(chan struct{})
	writer.onWait = func() { close(waiting) }
	key := store.ContentKey{0x11, 0x22, 0x33}
	admission := store.WriteAdmission{Generation: "G1"}
	type result struct {
		isNew bool
		err   error
	}
	leader := make(chan result, 1)
	follower := make(chan result, 1)
	go func() {
		isNew, err := writer.Put(context.Background(), admission, store.PartitionChunk, key, []byte("object"))
		leader <- result{isNew: isNew, err: err}
	}()
	select {
	case <-inner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not reach underlying writer")
	}
	go func() {
		isNew, err := writer.Put(context.Background(), admission, store.PartitionChunk, key, []byte("object"))
		follower <- result{isNew: isNew, err: err}
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("same-key follower did not join the in-flight Put")
	}
	close(inner.release)
	if got := <-leader; got.err != nil || !got.isNew {
		t.Fatalf("leader result = isNew %t err %v", got.isNew, got.err)
	}
	if got := <-follower; got.err != nil || got.isNew {
		t.Fatalf("follower result = isNew %t err %v", got.isNew, got.err)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("underlying same-key Put calls = %d, want 1", got)
	}
}

type parallelPublishStoreWriter struct {
	started chan store.ContentKey
	release chan struct{}
}

func (*parallelPublishStoreWriter) AdmitWrite(context.Context) (store.WriteAdmission, error) {
	return store.WriteAdmission{Generation: "G1"}, nil
}

func (*parallelPublishStoreWriter) PoolSize() int { return 2 }

func (w *parallelPublishStoreWriter) Put(ctx context.Context, _ store.WriteAdmission, _ store.Partition, key store.ContentKey, _ []byte) (bool, error) {
	select {
	case w.started <- key:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-w.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestManifestPublisherKeepsDifferentKeyPutsConcurrent(t *testing.T) {
	inner := &parallelPublishStoreWriter{started: make(chan store.ContentKey, 2), release: make(chan struct{})}
	writer := newDeduplicatingStoreWriter(inner)
	admission := store.WriteAdmission{Generation: "G1"}
	done := make(chan error, 2)
	for _, key := range []store.ContentKey{{0x11}, {0x22}} {
		key := key
		go func() {
			_, err := writer.Put(context.Background(), admission, store.PartitionChunk, key, []byte("object"))
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-inner.started:
		case <-time.After(5 * time.Second):
			t.Fatal("different-key Puts did not reach the underlying writer concurrently")
		}
	}
	close(inner.release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

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
	imageRef, _, err := sink.AbsorbImageSource(ctx, publishSource(t, publishEROFSFixture()))
	if err != nil {
		t.Fatal(err)
	}
	_, portable := publishPortable(t, "")
	portable.Boot.Root = config.PortableRootConfig{
		Base: imageRef, Overlay: &config.PortableOverlayConfig{Base: "self"},
	}
	runtimeBytes, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
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
	info, err := storage.Inspect(ctx, eResult.Ref, nil)
	if err != nil || info.Role != RoleSandbox {
		t.Fatalf("inspect E = %+v err=%v", info, err)
	}
	rootImage, err := manifest.ParseRef(info.Sandbox.Boot.Root.Base)
	if err != nil {
		t.Fatal(err)
	}
	if rootImage.Scheme != manifest.RefSchemeManifest {
		t.Fatalf("Manifest publisher root image ref = %s", rootImage.String())
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
	info, err = storage.Inspect(ctx, sResult.Ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Role != RoleSnapshot || info.Snapshot.SandboxRef == "" {
		t.Fatalf("inspect S = %+v", info)
	}
}
