package artifact

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type recordedPublish struct {
	role LogicalRole
	ref  string
	body []byte
}

type recordingPublishTarget struct {
	calls []recordedPublish
}

func (t *recordingPublishTarget) Put(ctx context.Context, role LogicalRole, source sparse.Source) (string, error) {
	body, err := readPublishSource(ctx, source)
	if err != nil {
		return "", err
	}
	ref := fmt.Sprintf("manifest://%064x", len(t.calls)+1)
	t.calls = append(t.calls, recordedPublish{role: role, ref: ref, body: body})
	return ref, nil
}

func (*recordingPublishTarget) Close() error { return nil }

func readPublishSource(ctx context.Context, source sparse.Source) ([]byte, error) {
	if source.Size() > uint64(int(^uint(0)>>1)) {
		return nil, errors.New("source too large")
	}
	body := make([]byte, int(source.Size()))
	for offset := 0; offset < len(body); {
		n, err := source.ReadAt(ctx, body[offset:], uint64(offset))
		offset += n
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if n == 0 && offset != len(body) {
			return nil, io.ErrUnexpectedEOF
		}
	}
	return body, nil
}

type recordedStream struct{ sparse.Source }

func (*recordedStream) Close() error { return nil }

func openRecordedSnapshot(t *testing.T, body []byte) (*snapshotfile.Root, *snapshot.Config) {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := snapshotfile.Open(context.Background(), &recordedStream{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := snapshot.ParseConfig(root.SnapshotConfig)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	return root, cfg
}

func openRecordedSandbox(t *testing.T, body []byte) *sandboxfile.Root {
	t.Helper()
	source, err := sparse.NewSource(bytes.NewReader(body), uint64(len(body)), nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(context.Background(), &recordedStream{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPublisherWritesMemorySandboxAndSnapshotBottomUp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	sink := snapshot.NewFileSink(dir, "fixture", nil, false, nil)

	parentCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion,
		// The parent is an opaque memory layer. Its historical Sandbox need not
		// exist when the current S explicitly carries the complete E graph.
		SandboxRef: "file://unavailable.sandbox@sha256:" + publishTestSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentLogical, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x21}, 4096)), []byte("{}"), []byte("{}"), parentCfg)
	if err != nil {
		t.Fatal(err)
	}
	parentRef, _, err := sink.AbsorbSnapshot(ctx, parentLogical)
	if err != nil {
		t.Fatal(err)
	}

	dependencyRef, _, err := sink.AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{0x32}, 4096)), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeBytes, _ := publishPortable(t, dependencyRef)
	eLogical, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x43}, 4096)), nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	eRef, _, err := sink.AbsorbSandbox(ctx, eLogical)
	if err != nil {
		t.Fatal(err)
	}

	rootCfg, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: eRef, FromRefs: []string{parentRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	rootLogical, err := snapshotfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x54}, 4096)), []byte("{}"), []byte("{}"), rootCfg)
	if err != nil {
		t.Fatal(err)
	}
	_, rootPath, err := sink.AbsorbSnapshot(ctx, rootLogical)
	if err != nil {
		t.Fatal(err)
	}

	target := &recordingPublishTarget{}
	publisher := newPublisher(storage, nil, target, nil)
	result, err := publisher.Publish(ctx, rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.Role != RoleSnapshot || result.Ref != target.calls[len(target.calls)-1].ref {
		t.Fatalf("result = %+v", result)
	}
	roles := make([]LogicalRole, len(target.calls))
	for i := range target.calls {
		roles[i] = target.calls[i].role
	}
	if want := []LogicalRole{RoleSnapshot, RoleOverlay, RoleSandbox, RoleSnapshot}; !reflect.DeepEqual(roles, want) {
		t.Fatalf("publish order = %v, want %v", roles, want)
	}

	parentRoot, publishedParent := openRecordedSnapshot(t, target.calls[0].body)
	if publishedParent.SandboxRef != "file://unavailable.sandbox@sha256:"+publishTestSHA {
		t.Fatalf("opaque memory parent was traversed or rewritten: %+v", publishedParent)
	}
	_ = parentRoot.Close()

	publishedE := openRecordedSandbox(t, target.calls[2].body)
	if publishedE.Portable.Boot.Root.Base != "self" {
		t.Fatalf("self was rewritten: %+v", publishedE.Portable.Boot.Root)
	}
	if len(publishedE.Portable.Boot.Root.BaseFromRefs) != 1 || publishedE.Portable.Boot.Root.BaseFromRefs[0] != target.calls[1].ref {
		t.Fatalf("E dependency graph = %+v", publishedE.Portable.Boot.Root)
	}
	_ = publishedE.Close()

	publishedSRoot, publishedS := openRecordedSnapshot(t, target.calls[3].body)
	defer publishedSRoot.Close()
	if publishedS.SandboxRef != target.calls[2].ref || len(publishedS.FromRefs) != 1 || publishedS.FromRefs[0] != target.calls[0].ref {
		t.Fatalf("S graph = %+v", publishedS)
	}
}

func TestPublisherRejectsLiveSandboxUsedAsEROFSBase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	sink := snapshot.NewFileSink(dir, "fixture", nil, false, nil)

	parentRuntime, _ := publishPortable(t, "")
	parentLogical, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x31}, 4096)), nil, parentRuntime,
	)
	if err != nil {
		t.Fatal(err)
	}
	parentRef, _, err := sink.AbsorbSandbox(ctx, parentLogical)
	if err != nil {
		t.Fatal(err)
	}

	_, current := publishPortable(t, "")
	current.Boot.Root = config.PortableRootConfig{
		Base: parentRef, Overlay: &config.PortableOverlayConfig{Base: "self"},
	}
	currentRuntime, err := config.MarshalPortableSandboxConfig(current)
	if err != nil {
		t.Fatal(err)
	}
	currentLogical, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x42}, 4096)), nil, currentRuntime,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, currentPath, err := sink.AbsorbSandbox(ctx, currentLogical)
	if err != nil {
		t.Fatal(err)
	}

	target := &recordingPublishTarget{}
	publisher := newPublisher(storage, nil, target, nil)
	_, err = publisher.Publish(ctx, currentPath)
	if err == nil || !strings.Contains(err.Error(), "EROFS") {
		t.Fatalf("publish live Sandbox root image error = %v", err)
	}
	if len(target.calls) != 0 {
		t.Fatalf("invalid root image emitted %d artifacts", len(target.calls))
	}
}

func TestPublisherPreservesRootImageConfig(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	sink := snapshot.NewFileSink(dir, "fixture", nil, false, nil)

	rootImagePath := filepath.Join(dir, "root.erofs")
	payload := make([]byte, 4096)
	binary.LittleEndian.PutUint32(payload[1024:1028], 0xE0F5E1E2)
	payload[1024+12] = 12
	binary.LittleEndian.PutUint32(payload[1024+36:1024+40], 1)
	if err := os.WriteFile(rootImagePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	want := &image.RuntimeConfig{Env: []string{"BUILT=yes"}, WorkingDir: "/home/user"}
	if err := image.AppendConfigZip(rootImagePath, want); err != nil {
		t.Fatal(err)
	}
	flattened, err := os.ReadFile(rootImagePath)
	if err != nil {
		t.Fatal(err)
	}
	rootImageRef, _, err := sink.AbsorbOverlaySource(ctx, publishSource(t, flattened))
	if err != nil {
		t.Fatal(err)
	}

	_, portable := publishPortable(t, "")
	portable.Boot.Root = config.PortableRootConfig{
		Base: rootImageRef,
		Overlay: &config.PortableOverlayConfig{
			Base: "self",
		},
	}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x42}, 4096)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, sandboxPath, err := sink.AbsorbSandbox(ctx, logical)
	if err != nil {
		t.Fatal(err)
	}

	target := &recordingPublishTarget{}
	publisher := newPublisher(storage, nil, target, nil)
	if _, err := publisher.Publish(ctx, sandboxPath); err != nil {
		t.Fatal(err)
	}
	if len(target.calls) != 2 || target.calls[0].role != RoleOverlay {
		t.Fatalf("publish calls = %+v", target.calls)
	}
	got, err := image.ReadConfig(bytes.NewReader(target.calls[0].body), int64(len(target.calls[0].body)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("published root image config = %#v, want %#v", got, want)
	}
}

func TestPublisherRejectsDiskRefUsedWithConflictingRolesBeforeOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	sink := snapshot.NewFileSink(dir, "fixture", nil, false, nil)

	conflictingRef := "file://shared.sandbox@sha256:" + publishTestSHA
	_, portable := publishPortable(t, "")
	portable.Boot.Root = config.PortableRootConfig{
		Base: conflictingRef,
		Overlay: &config.PortableOverlayConfig{
			Base: "self",
		},
	}
	portable.Boot.Disks = []config.PortableDiskConfig{{
		Name: "data",
		PortableRootConfig: config.PortableRootConfig{
			Base: conflictingRef,
		},
	}}
	portable.Mounts = []config.MountConfig{{Target: "/data", Type: "disk", Source: "data"}}
	runtimeConfig, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	logical, err := sandboxfile.BuildSource(
		publishSource(t, bytes.Repeat([]byte{0x51}, 4096)), nil, runtimeConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := sink.AbsorbSandbox(ctx, logical)
	if err != nil {
		t.Fatal(err)
	}

	target := &recordingPublishTarget{}
	publisher := newPublisher(storage, nil, target, nil)
	_, err = publisher.Publish(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "both root image and disk layer") {
		t.Fatalf("conflicting disk role error = %v", err)
	}
	if len(target.calls) != 0 {
		t.Fatalf("conflicting graph emitted %d artifacts", len(target.calls))
	}
}

func TestLocationPublisherCryptoAutoAndRequired(t *testing.T) {
	ctx := context.Background()
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	plainSink := snapshot.NewFileSink(sourceDir, "plain", nil, false, nil)
	runtimeBytes, _ := publishPortable(t, "")
	logical, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x65}, 4096)), nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, sourcePath, err := plainSink.AbsorbSandbox(ctx, logical)
	if err != nil {
		t.Fatal(err)
	}

	key := [32]byte{0x76, 0x77}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(key[:]))
	autoStorage, err := NewProcessStorage(&config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalAuto}})
	if err != nil {
		t.Fatal(err)
	}
	defer autoStorage.Close()
	locations := config.RefLocations{"published": targetDir}
	publisher, err := NewLocationPublisher(autoStorage, "published", targetDir, locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := publisher.Publish(ctx, sourcePath)
	closeErr := publisher.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("publish err=%v close=%v", err, closeErr)
	}
	ref, err := manifest.ParseRef(result.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Location != "published" || ref.DigestScheme != tarstream.DigestSchemeHMAC {
		t.Fatalf("auto published ref = %s", result.Ref)
	}
	path, err := locations.ResolveFile(ref, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetch.OpenTarStream(path); !errors.Is(err, tarstream.ErrCodecRequired) {
		t.Fatalf("published ciphertext without codec error = %v", err)
	}
	if document, err := autoStorage.Inspect(ctx, result.Ref, locations); err != nil || document.Role != RoleSandbox {
		t.Fatalf("inspect encrypted result = %+v err=%v", document, err)
	}

	requiredStorage, err := NewProcessStorage(&config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalRequired}})
	if err != nil {
		t.Fatal(err)
	}
	defer requiredStorage.Close()
	requiredPublisher, err := NewLocationPublisher(requiredStorage, "required", t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = requiredPublisher.Publish(ctx, sourcePath)
	_ = requiredPublisher.Close()
	if !errors.Is(err, tarstream.ErrPlaintextForbidden) {
		t.Fatalf("required plaintext error = %v", err)
	}
}

func TestPublisherHonorsCanceledContextWithoutOutput(t *testing.T) {
	dir := t.TempDir()
	runtimeBytes, _ := publishPortable(t, "")
	logical, err := sandboxfile.BuildSource(publishSource(t, bytes.Repeat([]byte{0x7a}, 4096)), nil, runtimeBytes)
	if err != nil {
		t.Fatal(err)
	}
	_, sourcePath, err := snapshot.NewFileSink(dir, "source", nil, false, nil).AbsorbSandbox(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	targetDir := t.TempDir()
	publisher, err := NewLocationPublisher(storage, "canceled", targetDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = publisher.Publish(ctx, sourcePath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publish error = %v", err)
	}
	entries, readErr := os.ReadDir(targetDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("canceled publish output = %v err=%v", entries, readErr)
	}
}

var _ fetch.Stream = (*recordedStream)(nil)
