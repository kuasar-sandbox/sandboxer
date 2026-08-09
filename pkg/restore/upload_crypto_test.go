package restore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/ingest"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"gopkg.in/yaml.v3"
)

func TestPublishLocalToLocationEncryptsLegacyGraph(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x61})
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	payload := bytes.Repeat([]byte{0x3c}, 64*1024)
	leafPath, leafTagged := writePublishArtifact(t, sourceDir, ".overlay", payload)
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{
		Base: "file://" + filepath.Base(leafPath) + "@sha256:" + strings.TrimPrefix(leafTagged, "sha256:"),
	}
	rootPath := writePublishSnapshot(t, sourceDir, cfg)

	rootRef, err := PublishLocalToLocation(ctx, rootPath, "encrypted", targetDir, codec, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	parsedRoot := mustParseRef(t, rootRef)
	if parsedRoot.DigestScheme != tarstream.DigestSchemeHMAC || parsedRoot.Location != "encrypted" {
		t.Fatalf("root ref=%#v", parsedRoot)
	}
	rootPhysical := filepath.Join(targetDir, parsedRoot.Path)
	if _, err := fetch.OpenTarStream(rootPhysical); !errors.Is(err, tarstream.ErrCodecRequired) {
		t.Fatalf("published root without codec error=%v", err)
	}
	rootIdentity := parsedRoot
	rootIdentity.Location = ""
	rootIdentity.Path = rootPhysical
	rootStream, _, _, err := openTarArtifact(rootPhysical, rootIdentity, codec, true)
	if err != nil {
		t.Fatal(err)
	}
	_, publishedCfg, err := readSnapshotEntries(ctx, rootStream, int64(rootStream.Size()))
	_ = rootStream.Close()
	if err != nil {
		t.Fatal(err)
	}
	leafRef := mustParseRef(t, publishedCfg.Boot.Root.Overlay.Base)
	if leafRef.DigestScheme != tarstream.DigestSchemeHMAC || leafRef.Location != "encrypted" || filepath.Base(leafRef.Path) != leafRef.Digest+".overlay" {
		t.Fatalf("published leaf ref=%#v", leafRef)
	}
	leafPhysical := filepath.Join(targetDir, leafRef.Path)
	if _, err := fetch.OpenTarStream(leafPhysical); !errors.Is(err, tarstream.ErrCodecRequired) {
		t.Fatalf("published leaf without codec error=%v", err)
	}
	leafIdentity := leafRef
	leafIdentity.Location = ""
	leafIdentity.Path = leafPhysical
	leafStream, _, _, err := openTarArtifact(leafPhysical, leafIdentity, codec, true)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if n, err := leafStream.ReadAt(ctx, got, 0); n != len(got) || err != nil {
		_ = leafStream.Close()
		t.Fatalf("leaf ReadAt=%d err=%v", n, err)
	}
	_ = leafStream.Close()
	if !bytes.Equal(got, payload) {
		t.Fatal("published encrypted leaf changed logical content")
	}
	if source, err := fetch.OpenTarStream(leafPath); err != nil {
		t.Fatalf("legacy source changed encoding: %v", err)
	} else {
		_ = source.Close()
	}
	if matches, _ := filepath.Glob(filepath.Join(targetDir, ".publish-*.tmp")); len(matches) != 0 {
		t.Fatalf("publisher left temporary files: %v", matches)
	}
}

func TestPublishLocalToLocationRequiredRejectsPlaintext(t *testing.T) {
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x62})
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	rootPath := writePublishSnapshot(t, t.TempDir(), cfg)
	if _, err := PublishLocalToLocation(context.Background(), rootPath, "encrypted", t.TempDir(), codec, true, nil); !errors.Is(err, tarstream.ErrPlaintextForbidden) {
		t.Fatalf("required plaintext publication error=%v", err)
	}
}

func TestPublishLocalToLocationFullyAuthenticatesInput(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x63})
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "1MiB"
	leafRef, _, err := snapshot.NewFileSink(sourceDir, "leaf", codec, false, nil).AbsorbOverlay(ctx, bytes.NewReader(bytes.Repeat([]byte{0x2a}, 4096)), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: leafRef}
	configBody, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	zipBody, err := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": configBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := bytes.Repeat([]byte{0x7d}, 1<<20)
	_, rootPath, err := snapshot.NewFileSink(sourceDir, "tampered", codec, false, nil).AbsorbBundle(ctx, bytes.NewReader(memory), nil, bytes.NewReader(zipBody))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/4] ^= 0x40
	if err := os.WriteFile(rootPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = PublishLocalToLocation(ctx, rootPath, "encrypted", targetDir, codec, false, nil)
	if !errors.Is(err, tarstream.ErrAuthentication) {
		t.Fatalf("tampered publication error=%v", err)
	}
	if entries, _ := os.ReadDir(targetDir); len(entries) != 0 {
		t.Fatalf("tampered input published files: %v", entries)
	}
}

func TestPublishEncryptedLeafPreservesLogicalIdentity(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x64})
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	payload := bytes.Repeat([]byte("deterministic"), 4096)
	tmp, err := os.CreateTemp(sourceDir, "leaf-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	scheme, digest, err := tarstream.WriteTo(ctx, tmp, "payload", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), tarstream.WithCodec(codec, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, digest+".overlay")
	if err := os.Rename(tmp.Name(), sourcePath); err != nil {
		t.Fatal(err)
	}
	expected := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: sourcePath, DigestScheme: scheme, Digest: digest}
	p := newSnapshotPublisher(ctx, codec, false, nil)
	p.location, p.directory = "encrypted", targetDir
	ref, err := p.publishLeaf("leaf", sourcePath, expected, publishLeafArtifact)
	if err != nil {
		t.Fatal(err)
	}
	parsed := mustParseRef(t, ref)
	if parsed.DigestScheme != scheme || parsed.Digest != digest {
		t.Fatalf("published identity=%s:%s want=%s:%s", parsed.DigestScheme, parsed.Digest, scheme, digest)
	}
	targetPath := filepath.Join(targetDir, parsed.Path)
	stream, err := fetch.OpenTarStream(
		targetPath,
		tarstream.WithCodec(codec, true),
		tarstream.WithExpectedDigest(parsed.DigestScheme, parsed.Digest),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := make([]byte, len(payload))
	if n, err := stream.ReadAt(ctx, got, 0); err != nil || n != len(got) {
		t.Fatalf("published ReadAt=%d err=%v", n, err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("published artifact logical content changed")
	}
}

func TestPublishLocationRepairsPlaintextExistingFinalInAuto(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x66})
	sourceDir := t.TempDir()
	targetDir := t.TempDir()
	payload := bytes.Repeat([]byte{0x38}, 32*1024)

	write := func(path string, encrypted bool) (string, string) {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		var options []tarstream.WriteOption
		if encrypted {
			options = append(options, tarstream.WithCodec(codec, false))
		}
		scheme, digest, err := tarstream.WriteTo(ctx, f, "payload", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), options...)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return scheme, digest
	}

	sourcePath := filepath.Join(sourceDir, "converted.tmp")
	scheme, digest := write(sourcePath, true)
	destination := filepath.Join(targetDir, digest+".overlay")
	plainScheme, _ := write(destination, false)
	if plainScheme != tarstream.DigestSchemeSHA256 {
		t.Fatalf("plaintext scheme=%q", plainScheme)
	}
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	p := newSnapshotPublisher(ctx, codec, false, nil)
	p.location, p.directory = "encrypted", targetDir
	if _, err := p.publishLocationFile(sourcePath, ".overlay", scheme, digest, uint64(len(payload))); err != nil {
		t.Fatalf("repair auto plaintext collision: %v", err)
	}
	after, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("plaintext published final was not replaced")
	}
	if err := validatePublishedFinal(ctx, destination, uint64(len(payload)), codec, true, scheme, digest); err != nil {
		t.Fatalf("repaired encrypted final is invalid: %v", err)
	}
}

func TestManifestPublisherFullyAuthenticatesInput(t *testing.T) {
	ctx := context.Background()
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x65})
	sourceDir := t.TempDir()
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "1MiB"
	configBody, _ := yaml.Marshal(cfg)
	zipBody, _ := snapshot.BuildZIP(map[string][]byte{
		"config.json": {}, "state.json": {}, "snapshot.cfg": configBody,
	})
	memory := bytes.Repeat([]byte{0x37}, 1<<20)
	_, rootPath, err := snapshot.NewFileSink(sourceDir, "tampered-upload", codec, false, nil).AbsorbBundle(ctx, bytes.NewReader(memory), nil, bytes.NewReader(zipBody))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/3] ^= 0x20
	if err := os.WriteFile(rootPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	p := newSnapshotPublisher(ctx, codec, false, nil)
	p.ing = consumingIngester{}
	if _, err := p.publishRootSnapshot(rootPath, manifest.Ref{}); !errors.Is(err, tarstream.ErrAuthentication) {
		t.Fatalf("tampered manifest publication error=%v", err)
	}
}

type consumingIngester struct{}

func (consumingIngester) Ingest(ctx context.Context, source sparse.Source, _ ingest.IngestOption) (*ingest.Result, error) {
	if err := consumeSource(ctx, source, 0); err != nil {
		return nil, err
	}
	return &ingest.Result{}, nil
}
