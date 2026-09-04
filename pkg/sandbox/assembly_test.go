package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/chunker"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storefs "github.com/kuasar-sandbox/accelerator/pkg/store/fs"
	"github.com/kuasar-sandbox/accelerator/pkg/store/pb"
	storeserver "github.com/kuasar-sandbox/accelerator/pkg/store/server"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

const assemblySHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type assemblyStream struct{ sparse.Source }

func (*assemblyStream) Close() error { return nil }

type assemblySparseSource struct {
	sparse.Source
	zero sparse.Extent
}

func (s *assemblySparseSource) RunAt(offset, limit uint64) (sparse.Run, error) {
	if offset >= s.zero.Offset && offset < s.zero.Offset+s.zero.Size {
		if limit == 0 {
			return nil, errors.New("zero run limit")
		}
		end := min(offset+limit, s.zero.Offset+s.zero.Size)
		return assemblyZeroRun{offset: offset, end: end}, nil
	}
	if offset < s.zero.Offset && limit > s.zero.Offset-offset {
		limit = s.zero.Offset - offset
	}
	return s.Source.RunAt(offset, limit)
}

type assemblyZeroRun struct {
	offset uint64
	end    uint64
}

func (r assemblyZeroRun) Offset() uint64       { return r.offset }
func (r assemblyZeroRun) End() uint64          { return r.end }
func (r assemblyZeroRun) Kind() sparse.RunKind { return sparse.Zero }
func (r assemblyZeroRun) ReadAt(_ context.Context, buffer []byte, inner uint64) (int, error) {
	if inner > r.end-r.offset || uint64(len(buffer)) > r.end-r.offset-inner {
		return 0, errors.New("zero run read is out of bounds")
	}
	clear(buffer)
	return len(buffer), nil
}

func assemblyFlattenedImage(t *testing.T) ([]byte, []byte, []sparse.Extent) {
	t.Helper()
	payload := make([]byte, 3*4096)
	binary.LittleEndian.PutUint32(payload[1024:1028], 0xE0F5E1E2)
	payload[1024+12] = 12
	binary.LittleEndian.PutUint32(payload[1024+36:1024+40], 3)
	path := filepath.Join(t.TempDir(), "flattened.img")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := image.AppendConfigZip(path, &image.RuntimeConfig{
		Cmd: []string{"/bin/service", "serve"}, Env: []string{"IMAGE=yes"}, WorkingDir: "/srv",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := sandboxfile.OpenFlattenedEROFS(context.Background(), &assemblyStream{Source: assemblySource(t, body, nil)})
	if err != nil {
		t.Fatal(err)
	}
	imageConfig := append([]byte(nil), opened.ImageConfig...)
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	return body, imageConfig, []sparse.Extent{{Offset: 4096, Size: 4096}}
}

func assemblySource(t *testing.T, body []byte, holes []sparse.Extent) sparse.Source {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), holes)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(body)) < 3*4096 {
		return source
	}
	return &assemblySparseSource{Source: source, zero: sparse.Extent{Offset: 2 * 4096, Size: 4096}}
}

func assemblyPortable() *config.PortableSandboxConfig {
	return &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 1, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "1GiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel:  "file://vmlinux@digest:" + assemblySHA,
			Runtime: "file://runtime.bundle@digest:" + assemblySHA,
			Root:    config.PortableRootConfig{Base: "self", Overlay: &config.PortableOverlayConfig{}},
		},
		Launch: config.PortableLaunchConfig{Exec: "/bin/service", Workdir: "/srv", Restart: "never"},
	}
}

func assemblyManifestStorage(t *testing.T) (*config.ManifestConfig, *artifact.ProcessStorage, [32]byte) {
	t.Helper()
	backend, err := storefs.New(storefs.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	server, err := storeserver.New(storeserver.Options{
		Backend: backend, Generations: func() []store.Generation { return []store.Generation{"G1"} }, VerifyKey: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "store.sock"))
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
	key := [32]byte{0x61, 0x62, 0x63}
	cfg := &config.ManifestConfig{
		Manifest: manifest.ManifestSubConfig{Key: hex.EncodeToString(key[:]), WriteGeneration: "G1"},
		Store:    manifest.StoreConfig{Endpoint: listener.Addr().String(), Pool: 2, Timeout: "5s"},
		Chunker:  chunker.Config{Mode: "fixed", Fixed: chunker.FixedConfig{Size: "4KiB"}},
		Crypto:   manifestcrypto.Config{Chunk: "aes", Manifest: "aes", Local: manifestcrypto.LocalOff},
	}
	storage, err := artifact.NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	return cfg, storage, key
}

func TestOpenAndAssembleSandboxEFromEveryImageCarrier(t *testing.T) {
	ctx := context.Background()
	body, imageConfig, holes := assemblyFlattenedImage(t)
	directImage, err := sandboxfile.OpenFlattenedEROFS(ctx, &assemblyStream{Source: assemblySource(t, body, holes)})
	if err != nil {
		t.Fatal(err)
	}
	directAssembly, err := AssembleSandboxE(ctx, directImage, assemblyPortable())
	if err != nil {
		_ = directImage.Close()
		t.Fatal(err)
	}
	directZero, err := directAssembly.RunAt(8192, 4096)
	if err != nil || directZero.Kind() != sparse.Zero {
		_ = directImage.Close()
		t.Fatalf("direct assembly zero run = %v, err=%v", directZero, err)
	}
	if err := directImage.Close(); err != nil {
		t.Fatal(err)
	}

	cfg, storage, key := assemblyManifestStorage(t)
	localDir := t.TempDir()
	localRef, localPath, err := snapshot.NewFileSink(localDir, "image", nil, false, nil).
		AbsorbImageSource(ctx, assemblySource(t, body, holes))
	if err != nil {
		t.Fatal(err)
	}
	localParsed, err := manifest.ParseRef(localRef)
	if err != nil {
		t.Fatal(err)
	}
	localParsed.Path = localPath
	localRef = localParsed.String()

	manifestPublisher, err := artifact.NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifestResult, publishErr := manifestPublisher.PublishSource(ctx, artifact.RoleImage, assemblySource(t, body, holes))
	closeErr := manifestPublisher.Close()
	if publishErr != nil || closeErr != nil {
		t.Fatalf("publish Manifest image err=%v close=%v", publishErr, closeErr)
	}

	bundleDir := t.TempDir()
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bundlePublisher, err := artifact.NewSingleRootBundlePublisher(
		cfg, func() ([32]byte, error) { return key, nil }, admission, "images", bundleDir, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	bundleResult, err := bundlePublisher.PublishSource(ctx, artifact.RoleImage, assemblySource(t, body, holes))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		ref       string
		locations config.RefLocations
		wantZero  bool
	}{
		{name: "local digest-qualified file", ref: localRef},
		{name: "manifest", ref: manifestResult.Ref, wantZero: true},
		{name: "located Bundle", ref: bundleResult.Ref, locations: config.RefLocations{"images": bundleDir}, wantZero: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			image, err := OpenFlattenedImage(ctx, test.ref, storage, test.locations)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(image.ImageConfig, imageConfig) {
				_ = image.Close()
				t.Fatal("OpenFlattenedImage changed config.json")
			}
			assembly, err := AssembleSandboxE(ctx, image, assemblyPortable())
			if err != nil {
				_ = image.Close()
				t.Fatal(err)
			}
			hole, err := assembly.RunAt(4096, 4096)
			if err != nil || hole.Kind() != sparse.Hole {
				_ = image.Close()
				t.Fatalf("hole run = %v, err=%v", hole, err)
			}
			zero, err := assembly.RunAt(8192, 4096)
			if err != nil || (test.wantZero && zero.Kind() != sparse.Zero) || (!test.wantZero && zero.Kind() != sparse.Data) {
				_ = image.Close()
				t.Fatalf("zero-valued run = %v, wantZero=%t, err=%v", zero, test.wantZero, err)
			}
			root, err := sandboxfile.Open(ctx, &assemblyStream{Source: assembly})
			if err != nil {
				_ = image.Close()
				t.Fatal(err)
			}
			if root.ArchiveBase != 3*4096 || !bytes.Equal(root.ImageConfig, imageConfig) || root.Portable.Boot.Root.Base != "self" || root.Portable.Boot.Root.Overlay == nil {
				_ = root.Close()
				_ = image.Close()
				t.Fatalf("strict Sandbox E = base %d root %+v", root.ArchiveBase, root.Portable.Boot.Root)
			}
			if err := root.Close(); err != nil {
				_ = image.Close()
				t.Fatal(err)
			}
			if err := image.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSandboxEAssemblyFailsClosedAndHonorsCancellation(t *testing.T) {
	body, imageConfig, _ := assemblyFlattenedImage(t)
	image, err := sandboxfile.OpenFlattenedEROFS(context.Background(), &assemblyStream{Source: assemblySource(t, body, nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	if _, err := PrepareSandboxEConfig(context.Background(), &config.SandboxConfig{}, []byte("not-json"), nil, nil, false, nil); err == nil {
		t.Fatal("malformed image config was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AssembleSandboxE(ctx, image, assemblyPortable()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled assembly error = %v", err)
	}
	if !bytes.Equal(image.ImageConfig, imageConfig) {
		t.Fatal("canceled assembly mutated image config")
	}
}
