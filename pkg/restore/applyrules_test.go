package restore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

// writeFile creates the new platform artifact shape used by ApplyRules tests:
// runtime paths get an aligned EROFS-prefix bundle, bases get tarstream.
func writeFile(t *testing.T, path string, body []byte) string {
	t.Helper()
	if strings.Contains(filepath.Base(path), "runtime") {
		return writeRuntimeBundle(t, path, body)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := tarstream.WriteTo(context.Background(), f, "image", sparse.Dense(bytes.NewReader(body), uint64(len(body))))
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return digest
}

func writeRuntimeBundle(t *testing.T, path string, body []byte) string {
	t.Helper()
	markerZIP := func(hexDigest string) []byte {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{
			Name:     tarstream.SHA256MarkerPrefix + hexDigest,
			Method:   zip.Store,
			Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		h.SetMode(0o444)
		if _, err := zw.CreateHeader(h); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	placeholder := markerZIP(strings.Repeat("0", 64))
	bundle := make([]byte, 2<<20)
	prefixSize := len(bundle) - len(placeholder)
	if len(body) > prefixSize {
		t.Fatalf("runtime test body too large: %d", len(body))
	}
	copy(bundle, body)
	sum := sha256.Sum256(bundle[:prefixSize])
	hexDigest := fmt.Sprintf("%x", sum[:])
	footer := markerZIP(hexDigest)
	if len(footer) != len(placeholder) {
		t.Fatal("runtime marker ZIP size changed")
	}
	copy(bundle[prefixSize:], footer)
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	return hexDigest
}

// baseSnap returns a SnapshotCfg with capacity 2vCPU/4GiB and the given
// runtime + base refs.
func baseSnap(runtimeRef, baseRef, overlayBase string) *SnapshotCfg {
	cfg := &SnapshotCfg{}
	cfg.Resources.Capacity.CPU = 2
	cfg.Resources.Capacity.Memory = "4GiB"
	cfg.Boot.RuntimeRef = runtimeRef
	cfg.Boot.Root.BaseRef = baseRef
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: overlayBase}
	return cfg
}

func applyRules(host *config.SandboxConfig, snap *SnapshotCfg, snapshotPath string) (*config.SandboxConfig, error) {
	return ApplyRules(host, snap, snapshotPath, nil, nil, false)
}

func TestApplyRulesCanonicalizesLegacyBaseToHMAC(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "runtime.erofs")
	runtimeDigest := writeFile(t, runtimePath, []byte("runtime body"))
	basePath := filepath.Join(dir, "base.erofs")
	plainDigest := writeFile(t, basePath, []byte("base body"))
	snap := baseSnap(
		"file://runtime.erofs@sha256:"+runtimeDigest,
		"file://base.erofs@sha256:"+plainDigest,
		"file://overlay",
	)
	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{}
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x71})
	out, err := ApplyRules(host, snap, filepath.Join(dir, "root.snapshot"), nil, codec, false)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(out.SnapshotRefs.BaseRef)
	if err != nil {
		t.Fatal(err)
	}
	if ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest == plainDigest {
		t.Fatalf("effective base ref=%#v", ref)
	}
	if _, err := ApplyRules(host, snap, filepath.Join(dir, "root.snapshot"), nil, codec, true); err == nil {
		t.Fatal("required ApplyRules accepted a legacy sha256 base ref")
	}
}

func TestCanonicalizeSnapshotTarRefsConvertsLegacyIdentities(t *testing.T) {
	dir := t.TempDir()
	path, tagged := writePublishArtifact(t, dir, ".snapshot", bytes.Repeat([]byte{0x37}, 4096))
	plainDigest := strings.TrimPrefix(tagged, "sha256:")
	legacy := "file://" + filepath.Base(path) + "@sha256:" + plainDigest
	manifestRef := "manifest://" + strings.Repeat("a", 64)
	cfg := &SnapshotCfg{FromRefs: []string{legacy, manifestRef}}
	cfg.Boot.Root.BaseRef = legacy
	cfg.Boot.Root.Overlay = &SnapOverlayCfg{Base: legacy, BaseFromRefs: []string{legacy}}
	cfg.Boot.Disks = []SnapDiskNode{
		{Base: legacy, BaseFromRefs: []string{legacy}},
		{BaseRef: legacy, Overlay: &SnapOverlayCfg{Base: legacy, BaseFromRefs: []string{legacy}}},
	}
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x73})
	opts := Options{SnapshotPath: filepath.Join(dir, "root.snapshot"), LocalCodec: codec}
	if err := canonicalizeSnapshotTarRefs(context.Background(), cfg, opts); err != nil {
		t.Fatal(err)
	}
	refs := []string{
		cfg.FromRefs[0], cfg.Boot.Root.BaseRef, cfg.Boot.Root.Overlay.Base,
		cfg.Boot.Root.Overlay.BaseFromRefs[0], cfg.Boot.Disks[0].Base,
		cfg.Boot.Disks[0].BaseFromRefs[0], cfg.Boot.Disks[1].BaseRef,
		cfg.Boot.Disks[1].Overlay.Base, cfg.Boot.Disks[1].Overlay.BaseFromRefs[0],
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
	if cfg.FromRefs[1] != manifestRef {
		t.Fatalf("manifest ref changed: %q", cfg.FromRefs[1])
	}
	requiredCfg := &SnapshotCfg{FromRefs: []string{legacy}}
	opts.LocalRequired = true
	if err := canonicalizeSnapshotTarRefs(context.Background(), requiredCfg, opts); err == nil {
		t.Fatal("required policy accepted a legacy snapshot graph ref")
	}
}

func TestCanonicalizeSnapshotTarRefsPreservesAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path, tagged := writePublishArtifact(t, dir, ".snapshot", bytes.Repeat([]byte{0x38}, 4096))
	plainDigest := strings.TrimPrefix(tagged, "sha256:")
	cfg := &SnapshotCfg{FromRefs: []string{"file://" + path + "@sha256:" + plainDigest}}
	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x75})
	if err := canonicalizeSnapshotTarRefs(context.Background(), cfg, Options{LocalCodec: codec}); err != nil {
		t.Fatal(err)
	}
	ref, err := manifest.ParseRef(cfg.FromRefs[0])
	if err != nil {
		t.Fatal(err)
	}
	if ref.Path != path {
		t.Fatalf("canonical path = %q, want %q", ref.Path, path)
	}
	if ref.DigestScheme != tarstream.DigestSchemeHMAC || ref.Digest == plainDigest {
		t.Fatalf("canonical identity = %#v", ref)
	}
}

func TestCanonicalizeSnapshotTarRefsDefersHostBaseOverrides(t *testing.T) {
	hostDir := t.TempDir()
	runtimePath := filepath.Join(hostDir, "runtime.erofs")
	runtimeDigest := writeFile(t, runtimePath, []byte("runtime"))
	rootPath := filepath.Join(hostDir, "root.img")
	rootDigest := writeFile(t, rootPath, []byte("root base"))
	diskPath := filepath.Join(hostDir, "data.img")
	diskDigest := writeFile(t, diskPath, []byte("data base"))

	rootRef := "file://root.img@sha256:" + rootDigest
	diskRef := "file://data.img@sha256:" + diskDigest
	snap := &SnapshotCfg{}
	snap.Boot.RuntimeRef = "file://runtime.erofs@sha256:" + runtimeDigest
	snap.Boot.Root.BaseRef = rootRef
	snap.Boot.Root.Overlay = &SnapOverlayCfg{}
	snap.Boot.Disks = []SnapDiskNode{{BaseRef: diskRef, Overlay: &SnapOverlayCfg{}}}
	host := &config.SandboxConfig{}
	host.Boot.Runtime = "file://" + runtimePath
	host.Boot.Root.Base = "file://" + rootPath
	host.Boot.Root.Overlay = &config.OverlayConfig{}
	host.Boot.Disks = []config.DiskConfig{{RootConfig: config.RootConfig{
		Base:    "file://" + diskPath,
		Overlay: &config.OverlayConfig{},
	}}}

	codec, _ := manifestcrypto.NewTarStreamCodec([32]byte{0x74})
	bundlePath := filepath.Join(t.TempDir(), "root.snapshot")
	opts := Options{SnapshotPath: bundlePath, HostCfg: host, LocalCodec: codec}
	if err := canonicalizeSnapshotTarRefs(context.Background(), snap, opts); err != nil {
		t.Fatal(err)
	}
	if snap.Boot.Root.BaseRef != rootRef || snap.Boot.Disks[0].BaseRef != diskRef {
		t.Fatalf("deferred base refs changed: root=%q disk=%q", snap.Boot.Root.BaseRef, snap.Boot.Disks[0].BaseRef)
	}

	merged, err := ApplyRules(host, snap, bundlePath, nil, codec, false)
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{merged.SnapshotRefs.BaseRef, merged.SnapshotRefs.DiskBaseRefs[0]}
	for i, raw := range refs {
		ref, err := manifest.ParseRef(raw)
		if err != nil {
			t.Fatalf("effective ref[%d]: %v", i, err)
		}
		if ref.DigestScheme != tarstream.DigestSchemeHMAC {
			t.Fatalf("effective ref[%d] scheme = %q, want hmac", i, ref.DigestScheme)
		}
	}
}

func TestApplyRules_CapacityMustMatchWhenProvided(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Resources.Capacity.CPU = 1 // mismatch
	host.Resources.Capacity.Memory = "2GiB"
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	if _, err := applyRules(host, snap, filepath.Join(dir, "ignored.snapshot")); err == nil || !strings.Contains(err.Error(), "capacity mismatch") {
		t.Fatalf("expected capacity mismatch error, got %v", err)
	}

	host.Resources.Capacity.CPU = 2
	host.Resources.Capacity.Memory = "4GiB"
	if _, err := applyRules(host, snap, filepath.Join(dir, "ignored.snapshot")); err != nil {
		t.Fatalf("matching capacity should be accepted: %v", err)
	}
}

func TestApplyRules_CapacityAutoFilledWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	out, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Resources.Capacity.CPU != 2 || out.Resources.Capacity.Memory != "4GiB" {
		t.Errorf("capacity not auto-filled: %+v", out.Resources.Capacity)
	}
}

func TestApplyRules_NoNetworkAllowed(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	out, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot"))
	if err != nil {
		t.Fatalf("no network source should be accepted: %v", err)
	}
	if out.Network.TAP != "" || out.Network.TapFD != nil {
		t.Fatalf("network source unexpectedly added: %+v", out.Network)
	}
}

func TestApplyRules_NetworkSourcesMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Network.TapFD = &config.TapFDConfig{Exec: []string{"helper"}}
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive network source error, got %v", err)
	}
}

func TestApplyRules_OverlayDiffOptional(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"

	// An empty overlay.diff is now allowed: restore auto-defaults a fresh diff
	// under the on-disk base dir (sized to the snapshot's overlay base).
	merged, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot"))
	if err != nil {
		t.Fatalf("empty overlay.diff should be accepted, got %v", err)
	}
	if merged.Boot.Root.Overlay.Diff != "" {
		t.Errorf("merged diff = %q, want empty (defaulted at runtime)", merged.Boot.Root.Overlay.Diff)
	}
}

func TestApplyRules_SingleDisk(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))

	// Single-disk snapshot.cfg: no base_ref, no overlay node; the captured root
	// diff is recorded at root.base with its chain at root.base_from_refs.
	snap := &SnapshotCfg{}
	snap.Resources.Capacity.CPU = 2
	snap.Resources.Capacity.Memory = "4GiB"
	snap.Boot.RuntimeRef = "file://runtime.erofs@sha256:" + rtDigest
	snap.Boot.Root.Base = "manifest://captured-diff"
	snap.Boot.Root.BaseFromRefs = []string{"manifest://lower1"}
	if !snap.SingleDisk() {
		t.Fatal("snapshot with no overlay node should report SingleDisk")
	}

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	out, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot"))
	if err != nil {
		t.Fatalf("ApplyRules single-disk: %v", err)
	}
	if !out.SingleDisk() {
		t.Error("merged config should be single-disk (Overlay nil)")
	}
	if out.Boot.Root.Base != "manifest://captured-diff" {
		t.Errorf("root.base = %q, want the captured diff", out.Boot.Root.Base)
	}
	if len(out.Boot.Root.BaseFromRefs) != 1 || out.Boot.Root.BaseFromRefs[0] != "manifest://lower1" {
		t.Errorf("root.base_from_refs = %v, want [manifest://lower1]", out.Boot.Root.BaseFromRefs)
	}
}

func TestApplyRules_RuntimeFileAutoResolveAndDigest(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snapPath := filepath.Join(dir, "x.snapshot")
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	out, err := applyRules(host, snap, snapPath)
	if err != nil {
		t.Fatalf("auto-resolve failed: %v", err)
	}
	wantRt := "file://" + rtPath
	if out.Boot.Runtime != wantRt {
		t.Errorf("runtime auto-resolve = %s, want %s", out.Boot.Runtime, wantRt)
	}
	wantBase := "file://" + bsPath + "@sha256:" + bsDigest
	if out.Boot.Root.Base != wantBase {
		t.Errorf("base auto-resolve = %s, want %s", out.Boot.Root.Base, wantBase)
	}
}

func TestApplyRules_RuntimeProvidedDigestMustMatch(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}
	host.Boot.Runtime = "file://" + rtPath
	host.Boot.Root.Base = "file://" + bsPath

	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err != nil {
		t.Fatalf("matching host file should pass: %v", err)
	}
	host.Boot.Runtime = "file://" + rtPath + "@sha256:" + strings.Repeat("f", 64)
	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err == nil || !strings.Contains(err.Error(), "host ref digest mismatch") {
		t.Fatalf("expected host ref digest failure, got %v", err)
	}
	host.Boot.Runtime = "file://" + rtPath

	// Tamper: overwrite runtime with different bytes; digest should mismatch.
	if err := os.WriteFile(rtPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest marker failure, got %v", err)
	}
}

func TestApplyRules_BasenameMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")

	// host points at a copy with a different filename
	otherPath := filepath.Join(dir, "renamed-runtime.erofs")
	if err := os.WriteFile(otherPath, []byte("runtime body"), 0o644); err != nil {
		t.Fatal(err)
	}

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}
	host.Boot.Runtime = "file://" + otherPath
	host.Boot.Root.Base = "file://" + bsPath

	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err == nil || !strings.Contains(err.Error(), "basename mismatch") {
		t.Fatalf("expected basename mismatch, got %v", err)
	}
}

func TestApplyRules_SchemeMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "file://abc.overlay")
	_ = rtPath
	_ = bsPath

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}
	host.Boot.Root.Base = "manifest://abcdef" // host says manifest, snap says file

	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err == nil || !strings.Contains(err.Error(), "scheme mismatch") {
		t.Fatalf("expected scheme mismatch, got %v", err)
	}
}

func TestApplyRules_RejectsLocatedRuntimeRef(t *testing.T) {
	snap := &SnapshotCfg{}
	snap.Boot.RuntimeRef = "file://runtime.bundle@sha256:" + strings.Repeat("a", 64) + "@location:platform"
	host := &config.SandboxConfig{}
	if _, err := applyRules(host, snap, "/snapshots/root.snapshot"); err == nil || !strings.Contains(err.Error(), "named ref locations are not supported") {
		t.Fatalf("located runtime_ref error = %v", err)
	}
}

func TestApplyRules_RejectsUnqualifiedRuntimeRef(t *testing.T) {
	snap := &SnapshotCfg{}
	snap.Boot.RuntimeRef = "file://runtime.bundle"
	host := &config.SandboxConfig{}
	if _, err := applyRules(host, snap, "/snapshots/root.snapshot"); err == nil || !strings.Contains(err.Error(), "digest qualifier is required") {
		t.Fatalf("unqualified runtime_ref error = %v", err)
	}
}

func TestApplyRules_CarriesEffectiveLocatedBaseRefs(t *testing.T) {
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "runtime.bundle")
	runtimeDigest := writeFile(t, runtimePath, []byte("runtime body"))
	rootDir := t.TempDir()
	rootPath, rootDigest := writePublishArtifact(t, rootDir, ".erofs", bytes.Repeat([]byte{0x51}, 4096))
	diskDir := t.TempDir()
	diskPath, diskDigest := writePublishArtifact(t, diskDir, ".erofs", bytes.Repeat([]byte{0x52}, 4096))
	rootName := filepath.Base(rootPath)
	diskName := filepath.Base(diskPath)

	snap := baseSnap(
		"file://runtime.bundle@sha256:"+runtimeDigest,
		"file://"+rootName+"@sha256:"+strings.TrimPrefix(rootDigest, "sha256:")+"@location:old-root",
		"manifest://"+strings.Repeat("a", 64),
	)
	snap.Boot.Disks = []SnapDiskNode{{
		BaseRef: "file://" + diskName + "@sha256:" + strings.TrimPrefix(diskDigest, "sha256:") + "@location:old-disk",
		Overlay: &SnapOverlayCfg{Base: "manifest://" + strings.Repeat("b", 64)},
	}}
	host := &config.SandboxConfig{}
	host.Boot.Root.Base = "file://" + rootName + "@location:new-root"
	host.Boot.Root.Overlay = &config.OverlayConfig{}
	host.Boot.Disks = []config.DiskConfig{{RootConfig: config.RootConfig{
		Base:    "file://" + diskName + "@location:new-disk",
		Overlay: &config.OverlayConfig{},
	}}}

	out, err := ApplyRules(host, snap, filepath.Join(dir, "root.snapshot"), config.RefLocations{
		"new-root": rootDir,
		"new-disk": diskDir,
	}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := "file://" + rootName + "@sha256:" + strings.TrimPrefix(rootDigest, "sha256:") + "@location:new-root"
	wantDisk := "file://" + diskName + "@sha256:" + strings.TrimPrefix(diskDigest, "sha256:") + "@location:new-disk"
	if out.SnapshotRefs.BaseRef != wantRoot {
		t.Fatalf("effective root base_ref = %q, want %q", out.SnapshotRefs.BaseRef, wantRoot)
	}
	if len(out.SnapshotRefs.DiskBaseRefs) != 1 || out.SnapshotRefs.DiskBaseRefs[0] != wantDisk {
		t.Fatalf("effective disk base_refs = %v, want [%s]", out.SnapshotRefs.DiskBaseRefs, wantDisk)
	}
}

func TestApplyRules_ManifestBaseMatchesKey(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	baseKey := strings.Repeat("a", 64)
	overlayKey := strings.Repeat("b", 64)
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "manifest://"+baseKey, "manifest://"+overlayKey)

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	// host empty: should auto-fill the snapshot manifest ref.
	out, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Boot.Root.Base != "manifest://"+baseKey {
		t.Errorf("manifest auto-fill = %s, want manifest://%s", out.Boot.Root.Base, baseKey)
	}
	if out.Boot.Root.Overlay.Base != "manifest://"+overlayKey {
		t.Errorf("overlay.base from snapshot = %s, want manifest://%s", out.Boot.Root.Overlay.Base, overlayKey)
	}

	// host provides matching manifest:// key
	host.Boot.Root.Base = "manifest://" + baseKey
	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err != nil {
		t.Fatalf("matching manifest key should pass: %v", err)
	}

	// host provides different manifest:// key → mismatch
	host.Boot.Root.Base = "manifest://" + strings.Repeat("c", 64)
	if _, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot")); err == nil || !strings.Contains(err.Error(), "manifest key mismatch") {
		t.Fatalf("expected manifest key mismatch, got %v", err)
	}
}

func TestApplyRules_OverlayBaseFromSnapshotIgnoresHost(t *testing.T) {
	dir := t.TempDir()
	rtPath := filepath.Join(dir, "runtime.erofs")
	rtDigest := writeFile(t, rtPath, []byte("runtime body"))
	bsPath := filepath.Join(dir, "base.erofs")
	bsDigest := writeFile(t, bsPath, []byte("base body"))
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "manifest://overlay-key-from-snap")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}
	host.Boot.Root.Overlay.Base = "manifest://something-else" // should be silently ignored

	out, err := applyRules(host, snap, filepath.Join(dir, "x.snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Boot.Root.Overlay.Base != "manifest://overlay-key-from-snap" {
		t.Errorf("overlay.base = %s, want manifest://overlay-key-from-snap (host ignored)", out.Boot.Root.Overlay.Base)
	}
}

func TestApplyRules_RuntimeManifestRejected(t *testing.T) {
	// runtime_ref must always be file:// per docs (boot.runtime is file://-only)
	snap := baseSnap("manifest://shouldnotbeallowed", "manifest://x", "manifest://y")
	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	if _, err := applyRules(host, snap, ""); err == nil || !strings.Contains(err.Error(), "runtime_ref") {
		t.Fatalf("expected runtime_ref scheme error, got %v", err)
	}
}

func TestApplyRules_ManifestBundleEmptyPathRejectsFileRefs(t *testing.T) {
	// When snapshotPath is empty (manifest-loaded bundle) and host doesn't
	// provide boot.runtime, we cannot auto-resolve a file:// ref → error.
	rtDigest := strings.Repeat("ab", 32)
	bsDigest := strings.Repeat("cd", 32)
	snap := baseSnap("file://runtime.erofs@sha256:"+rtDigest, "file://base.erofs@sha256:"+bsDigest, "manifest://overlay-key")

	host := &config.SandboxConfig{}
	host.Network.TAP = "tap0"
	host.Boot.Root.Overlay = &config.OverlayConfig{Diff: "file:///tmp/diff"}

	if _, err := applyRules(host, snap, ""); err == nil || !strings.Contains(err.Error(), "boot.runtime") {
		t.Fatalf("expected boot.runtime explicit-required error, got %v", err)
	}
}
