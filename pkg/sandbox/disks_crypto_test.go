package sandbox

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestOpenDiskStreamLocalCryptoMatrix(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := [32]byte{1, 2, 3, 4}
	codec, err := manifestcrypto.NewTarStreamCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := manifestcrypto.NewTarStreamCodec([32]byte{9, 8, 7})
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("local-crypto"), 1024)
	plainPath, plainScheme, plainDigest := writeDiskArtifact(t, dir, "image", payload, nil)
	encryptedPath, encryptedScheme, encryptedDigest := writeDiskArtifact(t, dir, "image", payload, codec)
	if plainScheme != tarstream.DigestSchemeSHA256 || encryptedScheme != tarstream.DigestSchemeHMAC {
		t.Fatalf("schemes plain=%q encrypted=%q", plainScheme, encryptedScheme)
	}

	plainRef := fileRef(plainPath, plainScheme, plainDigest)
	encryptedRef := fileRef(encryptedPath, encryptedScheme, encryptedDigest)
	open := func(raw string, selected tarstream.Codec, required bool) (string, string, error) {
		stream, _, err := OpenDiskStream(ctx, raw, nil, nil, selected, required)
		if err != nil {
			return "", "", err
		}
		defer stream.Close()
		digester := stream.(tarstream.Digester)
		scheme, digest := digester.Digest()
		return scheme, digest, nil
	}

	if scheme, digest, err := open(plainRef, nil, false); err != nil || scheme != plainScheme || digest != plainDigest {
		t.Fatalf("off plaintext = %s:%s err=%v", scheme, digest, err)
	}
	if _, _, err := open(encryptedRef, nil, false); err == nil {
		t.Fatal("off accepted an hmac/encrypted artifact")
	}
	wantKeyed := codec.KeyedDigest(mustDigestBytes(t, plainDigest))
	wantHMAC := hex.EncodeToString(wantKeyed[:])
	if scheme, digest, err := open(plainRef, codec, false); err != nil || scheme != tarstream.DigestSchemeHMAC || digest != wantHMAC {
		t.Fatalf("auto legacy plaintext = %s:%s err=%v", scheme, digest, err)
	}
	if scheme, digest, err := open(encryptedRef, codec, false); err != nil || scheme != encryptedScheme || digest != encryptedDigest {
		t.Fatalf("auto encrypted = %s:%s err=%v", scheme, digest, err)
	}
	if scheme, digest, err := open(encryptedRef, codec, true); err != nil || scheme != encryptedScheme || digest != encryptedDigest {
		t.Fatalf("required encrypted = %s:%s err=%v", scheme, digest, err)
	}
	if _, _, err := open(plainRef, codec, true); err == nil {
		t.Fatal("required accepted a legacy sha256 ref")
	}
	if _, _, err := open(encryptedRef, wrong, true); !errors.Is(err, tarstream.ErrAuthentication) {
		t.Fatalf("wrong key error = %v", err)
	}
	if _, _, err := open(encryptedRef, nil, true); err == nil {
		t.Fatal("required policy accepted a nil codec")
	}

	// hmac is an identity scheme, not an encoding flag: auto accepts a
	// historical plaintext artifact when the ref carries its key-bound identity.
	plainHMACRef := fileRef(plainPath, tarstream.DigestSchemeHMAC, wantHMAC)
	if _, _, err := open(plainHMACRef, codec, false); err != nil {
		t.Fatalf("auto plaintext @hmac: %v", err)
	}
	if _, _, err := open(plainHMACRef, codec, true); !errors.Is(err, tarstream.ErrPlaintextForbidden) {
		t.Fatalf("required plaintext @hmac error = %v", err)
	}
	body, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}
	body[0] ^= 0x80
	if err := os.WriteFile(plainPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := open(plainRef, codec, false); err == nil || strings.Contains(err.Error(), plainDigest) {
		t.Fatalf("key-bound error exposed legacy plain digest: %v", err)
	}
}

func TestOpenDiskStreamBareAndLocatedIdentityRules(t *testing.T) {
	ctx := context.Background()
	key := [32]byte{0x42}
	codec, _ := manifestcrypto.NewTarStreamCodec(key)
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x5a}, 8192)
	plainPath, _, _ := writeDiskArtifact(t, dir, "image", payload, nil)
	encryptedPath, scheme, digest := writeDiskArtifact(t, dir, "image", payload, codec)

	for _, path := range []string{plainPath, encryptedPath} {
		stream, _, err := OpenDiskStream(ctx, "file://"+path, nil, nil, codec, false)
		if err != nil {
			t.Fatalf("auto bare %s: %v", filepath.Base(path), err)
		}
		_ = stream.Close()
	}
	if _, _, err := OpenDiskStream(ctx, "file://"+plainPath, nil, nil, codec, true); err == nil {
		t.Fatal("required accepted a bare legacy plaintext artifact")
	}

	logicalName := "logical-root.image"
	logicalPath := filepath.Join(dir, logicalName)
	body, err := os.ReadFile(encryptedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logicalPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	ref := manifest.Ref{
		Scheme: manifest.RefSchemeFile, Path: logicalName, Location: "shared",
		DigestScheme: scheme, Digest: digest,
	}
	stream, _, err := OpenDiskStream(ctx, ref.String(), nil, config.RefLocations{"shared": dir}, codec, true)
	if err != nil {
		t.Fatalf("explicit located logical basename: %v", err)
	}
	_ = stream.Close()
	bare := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: logicalName, Location: "shared"}
	if _, _, err := OpenDiskStream(ctx, bare.String(), nil, config.RefLocations{"shared": dir}, codec, false); err == nil {
		t.Fatal("bare logical basename bypassed content-address validation")
	}
}

func TestBuildDiskRefExportsPolicyIdentity(t *testing.T) {
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x43})
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x2a}, 4096)
	plainPath, _, plainDigest := writeDiskArtifact(t, dir, "image", payload, nil)
	legacy := fileRef(plainPath, tarstream.DigestSchemeSHA256, plainDigest)
	canonical, err := buildDiskRef(legacy, nil, codec, false)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest == plainDigest {
		t.Fatalf("canonical legacy ref=%#v", ref)
	}
	if ref.Path != filepath.Base(plainPath) {
		t.Fatalf("canonical path=%q want=%q", ref.Path, filepath.Base(plainPath))
	}
	if _, err := buildDiskRef(legacy, nil, codec, true); err == nil {
		t.Fatal("required buildDiskRef accepted a legacy sha256 ref")
	}
}

func TestCanonicalizeConfiguredTarRefsConvertsColdChains(t *testing.T) {
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x45})
	dir := t.TempDir()
	path, _, plainDigest := writeDiskArtifact(t, dir, "image", bytes.Repeat([]byte{0x31}, 4096), nil)
	legacy := fileRef(path, tarstream.DigestSchemeSHA256, plainDigest)
	manifestRef := "manifest://" + strings.Repeat("a", 64)
	cfg := &config.SandboxConfig{}
	cfg.Boot.Root.Base = legacy
	cfg.Boot.Root.BaseFromRefs = []string{legacy, manifestRef}
	cfg.Boot.Disks = []config.DiskConfig{{RootConfig: config.RootConfig{
		Base: legacy,
		Overlay: &config.OverlayConfig{
			Base:         legacy,
			BaseFromRefs: []string{legacy},
		},
	}}}
	if err := canonicalizeConfiguredTarRefs(cfg, nil, codec, false); err != nil {
		t.Fatal(err)
	}
	refs := []string{
		cfg.Boot.Root.Base, cfg.Boot.Root.BaseFromRefs[0], cfg.Boot.Disks[0].Base,
		cfg.Boot.Disks[0].Overlay.Base, cfg.Boot.Disks[0].Overlay.BaseFromRefs[0],
	}
	for i, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			t.Fatalf("ref[%d]: %v", i, err)
		}
		if ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest == plainDigest {
			t.Fatalf("ref[%d] was not canonicalized: %#v", i, ref)
		}
	}
	if cfg.Boot.Root.BaseFromRefs[1] != manifestRef {
		t.Fatalf("manifest ref changed: %q", cfg.Boot.Root.BaseFromRefs[1])
	}
	required := &config.SandboxConfig{}
	required.Boot.Root.Base = legacy
	if err := canonicalizeConfiguredTarRefs(required, nil, codec, true); err == nil {
		t.Fatal("required policy accepted a legacy cold-chain ref")
	}
}

func TestCanonicalizeConfiguredTarRefsAcceptsNamedNodeLocalArtifact(t *testing.T) {
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x46})
	dir := t.TempDir()
	plainPath, _, plainDigest := writeDiskArtifact(t, dir, "image", bytes.Repeat([]byte{0x31}, 4096), nil)
	namedPlainPath := filepath.Join(dir, "legacy.erofs")
	if err := os.Rename(plainPath, namedPlainPath); err != nil {
		t.Fatal(err)
	}
	plainCfg := &config.SandboxConfig{}
	plainCfg.Boot.Root.Base = "file://" + namedPlainPath
	if err := canonicalizeConfiguredTarRefs(plainCfg, nil, codec, false); err != nil {
		t.Fatal(err)
	}
	plainRef, err := manifest.ParseRef(plainCfg.Boot.Root.Base)
	if err != nil {
		t.Fatal(err)
	}
	if plainRef.Path != namedPlainPath || plainRef.DigestScheme != tarstream.DigestSchemeHMAC || plainRef.Digest == plainDigest {
		t.Fatalf("canonical named plaintext ref=%#v", plainRef)
	}

	contentPath, _, _ := writeDiskArtifact(t, dir, "image", bytes.Repeat([]byte{0x32}, 4096), codec)
	namedPath := filepath.Join(dir, "app.erofs")
	if err := os.Rename(contentPath, namedPath); err != nil {
		t.Fatal(err)
	}
	cfg := &config.SandboxConfig{}
	cfg.Boot.Root.Base = "file://" + namedPath
	if err := canonicalizeConfiguredTarRefs(cfg, nil, codec, true); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(cfg.Boot.Root.Base)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Path != namedPath || ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest == "" {
		t.Fatalf("canonical named local ref=%#v", ref)
	}
}

func TestOpenDiskStreamRejectsEncryptedEnvelopeDamage(t *testing.T) {
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x44})
	dir := t.TempDir()
	path, scheme, digest := writeDiskArtifact(t, dir, "image", bytes.Repeat([]byte{0x3d}, 64*1024), codec)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	refFor := func(path string) string { return fileRef(path, scheme, digest) }
	cases := map[string]func([]byte) []byte{
		"version": func(body []byte) []byte {
			body[8] ^= 0x7f
			return body
		},
		"header": func(body []byte) []byte {
			body[32] ^= 0x40
			return body
		},
		"suffix": func(body []byte) []byte {
			body[len(body)-32] ^= 0x20
			return body
		},
		"truncated": func(body []byte) []byte { return body[:len(body)-1] },
		"extra":     func(body []byte) []byte { return append(body, 0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			body := mutate(append([]byte(nil), original...))
			damaged := filepath.Join(dir, "damaged-"+name+".image")
			if err := os.WriteFile(damaged, body, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := OpenDiskStream(context.Background(), refFor(damaged), nil, nil, codec, true); err == nil {
				t.Fatal("damaged encrypted envelope was accepted")
			}
		})
	}
}

func writeDiskArtifact(t *testing.T, dir, name string, payload []byte, codec tarstream.Codec) (string, string, string) {
	t.Helper()
	tmp, err := os.CreateTemp(dir, "artifact-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	var options []tarstream.WriteOption
	if codec != nil {
		options = append(options, tarstream.WithCodec(codec, false))
	}
	scheme, digest, err := tarstream.WriteTo(context.Background(), tmp, name, sparse.Dense(bytes.NewReader(payload), uint64(len(payload))), options...)
	if err != nil {
		_ = tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, digest+"."+name)
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	return path, scheme, digest
}

func fileRef(path, scheme, digest string) string {
	return (&manifest.Ref{Scheme: manifest.RefSchemeFile, Path: path, DigestScheme: scheme, Digest: digest}).String()
}

func mustDigestBytes(t *testing.T, value string) [32]byte {
	t.Helper()
	var result [32]byte
	if _, err := hex.Decode(result[:], []byte(value)); err != nil {
		t.Fatal(err)
	}
	return result
}
