package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func testKernelRef(name, digest string) string { return "file://" + name + "@digest:" + digest }

func TestKernelChecksumParser(t *testing.T) {
	digest := strings.Repeat("A1", 32)
	for _, record := range []string{digest, digest + "\n", digest + "\r\n", digest + "  vmlinux\n", digest + " *vmlinux\r\n"} {
		got, err := parseKernelChecksum([]byte(record), "vmlinux")
		if err != nil || got != strings.ToLower(digest) {
			t.Fatalf("record %q: %q, %v", record, got, err)
		}
	}
	for _, record := range []string{"", "\n", digest[:63], "g" + digest[1:], digest + "\n\n", digest + "\n" + digest,
		digest + "  other", digest + "  ./vmlinux", digest + "  /tmp/vmlinux", digest + "  vmlinux extra",
		digest + " *other", digest + " vmlinux", "\\" + digest + "  vmlinux", digest + "\r", digest + "\r\nextra"} {
		if got, err := parseKernelChecksum([]byte(record), "vmlinux"); err == nil {
			t.Fatalf("accepted %q as %q", record, got)
		}
	}
}

func TestKernelChecksumLookupAndFallback(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("actual bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	uri := "file://" + kernel
	sidecar := kernel + ".sha256"
	actual := sha256.Sum256([]byte("actual bytes"))
	got, err := buildKernelRef(uri)
	if err != nil || got != testKernelRef("vmlinux", hex.EncodeToString(actual[:])) {
		t.Fatalf("fallback = %q, %v", got, err)
	}
	if ref, present, err := optionalKernelRef(uri); err != nil || present || ref != "" {
		t.Fatalf("absent sidecar = %q, %v, %v", ref, present, err)
	}
	metadata := strings.Repeat("B", 64)
	if err := os.WriteFile(sidecar, []byte(metadata+"  vmlinux\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = buildKernelRef(uri)
	if err != nil || got != testKernelRef("vmlinux", strings.ToLower(metadata)) {
		t.Fatalf("metadata was recomputed or ignored: %q, %v", got, err)
	}
	if err := os.WriteFile(sidecar, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyKernelArtifact(uri); err != nil {
		t.Fatalf("cheap restore preflight consulted sidecar: %v", err)
	}
	if _, err := buildKernelRef(uri); err == nil {
		t.Fatal("invalid present metadata fell back to hash")
	}
	for _, bad := range []string{"file://vmlinux", uri + "@digest:" + strings.Repeat("a", 64)} {
		if _, err := buildKernelRef(bad); err == nil {
			t.Fatalf("accepted host binding %q", bad)
		}
	}
}

func TestKernelChecksumRejectsPresentBadInputs(t *testing.T) {
	for _, kind := range []string{"directory", "fifo", "oversize", "unreadable", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			kernel := filepath.Join(dir, "vmlinux")
			if err := os.WriteFile(kernel, []byte("kernel"), 0600); err != nil {
				t.Fatal(err)
			}
			path := kernel + ".sha256"
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "oversize":
				err = os.WriteFile(path, []byte(strings.Repeat("a", kernelSidecarLimit+1)), 0600)
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("root may read mode-000 files; run this case unprivileged")
				}
				err = os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0000)
			case "dangling":
				err = os.Symlink(filepath.Join(dir, "missing"), path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, present, err := optionalKernelRef("file://" + kernel); err == nil || present {
				t.Fatalf("%s accepted or treated as absent: %v/%v", kind, present, err)
			}
		})
	}
}

func TestSDKRetainsArtifactIdentityThroughLaunch(t *testing.T) {
	shape, launch := sdkFixture(t)
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	bundle := strings.TrimPrefix(shape.Bundle, "file://")
	digest := strings.Repeat("b", 64) // deliberately differs from kernel payload
	if err := os.WriteFile(kernel+".sha256", []byte(strings.ToUpper(digest)+" *vmlinux\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.hostProjection.KernelRef != testKernelRef("vmlinux", digest) || r.hostProjection.RuntimeRef == "" || r.legacyUnknownKernel {
		t.Fatalf("startup identity = %+v, legacy=%v", r.hostProjection, r.legacyUnknownKernel)
	}
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	want, err := config.ProjectPortableCold(cfg, r.hostProjection)
	if err != nil {
		t.Fatal(err)
	}
	// Identity paths disappear after RuntimeReady. Launch must use the retained
	// values; the child fixture does not load either artifact as a real VM.
	for _, path := range []string{kernel, kernel + ".sha256", bundle} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Launch(ctx, launch); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.runDir, config.SandboxRuntimeConfigName))
	if err != nil {
		t.Fatal(err)
	}
	got, err := config.ParsePortableSandboxConfig(raw)
	if err != nil || got.Boot.Kernel != want.Boot.Kernel || got.Boot.Runtime != want.Boot.Runtime {
		t.Fatalf("C0 identities = %+v, want %+v: %v", got, want, err)
	}
}

func TestSDKPortableIdentityComparison(t *testing.T) {
	for _, kind := range []string{"matching", "kernel", "runtime", "basename"} {
		t.Run(kind, func(t *testing.T) {
			shape, launch := sdkFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			cfg, err := launch.config(shape.config())
			if err != nil {
				t.Fatal(err)
			}
			portable, err := config.ProjectPortableCold(cfg, r.hostProjection)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "kernel":
				portable.Boot.Kernel = testKernelRef("vmlinux", strings.Repeat("b", 64))
			case "runtime":
				portable.Boot.Runtime = testKernelRef("runtime.bundle", strings.Repeat("b", 64))
			case "basename":
				portable.Boot.Kernel = strings.Replace(portable.Boot.Kernel, "file://vmlinux@", "file://other@", 1)
			}
			original, err := json.Marshal(portable)
			if err != nil {
				t.Fatal(err)
			}
			launch.PortableConfig = portable
			err = r.Launch(ctx, launch)
			if kind == "matching" && err != nil || kind != "matching" && err == nil {
				t.Fatalf("Launch(%s) = %v", kind, err)
			}
			after, marshalErr := json.Marshal(portable)
			if marshalErr != nil || !bytes.Equal(after, original) {
				t.Fatal("caller C0 mutated")
			}
			if kind != "matching" && r.State() != "closed" {
				t.Fatalf("failure lifecycle = %s", r.State())
			}
		})
	}
}

func testLegacyRunAtReady(t *testing.T, shape RuntimeSpec, cfg *config.SandboxConfig, portable *config.PortableSandboxConfig, sourceRef string) *config.PortableSandboxConfig {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan *config.PortableSandboxConfig, 1)
	opts := RunOptions{Cfg: cfg, SandboxID: shape.SandboxID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot,
		PortableConfig: portable, NotifyReadiness: func(event ReadinessEvent) {
			if event != ReadinessReady {
				return
			}
			raw, err := os.ReadFile(filepath.Join(shape.RuntimeRoot, shape.SandboxID, config.SandboxRuntimeConfigName))
			if err != nil {
				ready <- nil
			} else {
				parsed, _ := config.ParsePortableSandboxConfig(raw)
				ready <- parsed
			}
			cancel()
		}}
	if portable != nil {
		opts.SourceBinding = &RunSourceBinding{SandboxRef: sourceRef, RuntimeRef: sourceRef}
	}
	_, _ = Run(ctx, opts)
	select {
	case got := <-ready:
		if got == nil {
			t.Fatal("legacy Run did not write a valid C0 before ready")
		}
		return got
	default:
		t.Fatal("legacy Run did not reach ready")
		return nil
	}
}

func TestLegacyRunColdAndPortableIdentityHandoff(t *testing.T) {
	for _, mode := range []string{"cold-sidecar", "cold-fallback", "portable-absent"} {
		t.Run(mode, func(t *testing.T) {
			shape, launch := sdkFixture(t)
			cfg, err := launch.config(shape.config())
			if err != nil {
				t.Fatal(err)
			}
			kernel := strings.TrimPrefix(shape.Kernel, "file://")
			var portable *config.PortableSandboxConfig
			if mode == "cold-sidecar" {
				if err := os.WriteFile(kernel+".sha256", []byte(strings.Repeat("B", 64)), 0600); err != nil {
					t.Fatal(err)
				}
			} else if mode == "portable-absent" {
				ids, err := ResolvePortableProjection(cfg)
				if err != nil {
					t.Fatal(err)
				}
				portable, err = config.ProjectPortableCold(cfg, ids)
				if err != nil {
					t.Fatal(err)
				}
				portable.Boot.Root.Base = "self"
				cfg.Boot.Root.Base = "self"
				// The existing C0 is ancestry. With no sidecar, portable Run does
				// not promote its expected digest into a verified host fact.
				if err := os.WriteFile(kernel, []byte("changed host kernel"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			sourceRef := ""
			if portable != nil {
				path, scheme, digest := writeDiskArtifact(t, t.TempDir(), "image", lifecycleEROFSFixture(), nil)
				sourceRef = fileRef(path, scheme, digest)
			}
			watch := watchArtifactAccess(t, kernel)
			got := testLegacyRunAtReady(t, shape, cfg, portable, sourceRef)
			wantReads := 0
			if mode == "cold-fallback" {
				wantReads = 1
			}
			if gotReads := watch.count(unix.IN_ACCESS); gotReads != wantReads {
				t.Fatalf("legacy %s kernel payload reads = %d, want %d", mode, gotReads, wantReads)
			}
			if mode == "cold-sidecar" && got.Boot.Kernel != testKernelRef("vmlinux", strings.Repeat("b", 64)) {
				t.Fatalf("cold Run ignored sidecar: %s", got.Boot.Kernel)
			}
			if portable != nil && got.Boot.Kernel != portable.Boot.Kernel {
				t.Fatalf("portable Run changed ancestry: %s vs %s", got.Boot.Kernel, portable.Boot.Kernel)
			}
		})
	}
}

func TestLegacyPortableRunChecksPresentMetadataBeforeVM(t *testing.T) {
	shape, launch := sdkFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := ResolvePortableProjection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := config.ProjectPortableCold(cfg, ids)
	if err != nil {
		t.Fatal(err)
	}
	portable.Boot.Root.Base = "self"
	cfg.Boot.Root.Base = "self"
	path, scheme, digest := writeDiskArtifact(t, t.TempDir(), "image", lifecycleEROFSFixture(), nil)
	sourceRef := fileRef(path, scheme, digest)
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	for _, body := range []string{strings.Repeat("b", 64), "invalid"} {
		if err := os.WriteFile(kernel+".sha256", []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := Run(context.Background(), RunOptions{Cfg: cfg, PortableConfig: portable,
			SourceBinding: &RunSourceBinding{SandboxRef: sourceRef, RuntimeRef: sourceRef},
			SandboxID:     shape.SandboxID, CHBinary: shape.CHBinary, RuntimeRoot: shape.RuntimeRoot, BaseRoot: shape.BaseRoot})
		if err == nil {
			t.Fatalf("portable Run accepted sidecar %q", body)
		}
		if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !os.IsNotExist(err) {
			t.Fatalf("VM resources created before predictable identity error: %v", err)
		}
	}
}

func TestPublicStartWithPortableRequiresResolvedHostKernel(t *testing.T) {
	shape, launch := sdkFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	old, err := ResolvePortableProjection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	launch.PortableConfig, err = config.ProjectPortableCold(cfg, old)
	if err != nil {
		t.Fatal(err)
	}
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	if err := os.WriteFile(kernel, []byte("new kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch}); r != nil || err == nil || !strings.Contains(err.Error(), "boot.kernel identity mismatch") {
		t.Fatalf("public portable Start = %v/%v", r, err)
	}
	if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !os.IsNotExist(err) {
		t.Fatalf("failed Start retained runtime resources: %v", err)
	}
}

func TestSDKNewRuntimeResolvesChangedMetadata(t *testing.T) {
	shape, _ := sdkFixture(t)
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	path := kernel + ".sha256"
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if err := os.WriteFile(path, []byte(strings.Repeat("b", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := StartRuntime(ctx, shape)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.hostProjection.KernelRef != testKernelRef("vmlinux", strings.Repeat("a", 64)) || second.hostProjection.KernelRef != testKernelRef("vmlinux", strings.Repeat("b", 64)) {
		t.Fatalf("identities did not remain per-runtime: %s, %s", first.hostProjection.KernelRef, second.hostProjection.KernelRef)
	}
}

func TestPreparedStartupRejectsDifferentBindingsAndUnknownOrdinary(t *testing.T) {
	shape, launch := sdkFixture(t)
	ids, err := ResolvePortableProjection(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedHostIdentity{kernel: shape.Kernel, bundle: shape.Bundle, projection: ids}
	changed := shape
	changed.Kernel = "file://" + filepath.Join(shape.RuntimeRoot, "other")
	if _, err := startRuntime(context.Background(), changed, prepared); err == nil || !strings.Contains(err.Error(), "bindings differ") {
		t.Fatalf("accepted different prepared binding: %v", err)
	}
	unknown := *prepared
	unknown.projection.KernelRef = ""
	unknown.unknownKernel = true
	if _, err := start(context.Background(), SandboxSpec{Runtime: shape, Launch: launch}, &unknown); err == nil || !strings.Contains(err.Error(), "legacy portable boot") {
		t.Fatalf("unknown kernel entered ordinary Launch: %v", err)
	}
	if _, err := startRuntime(context.Background(), shape, &preparedHostIdentity{kernel: shape.Kernel, bundle: shape.Bundle}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("accepted incomplete prepared identity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !os.IsNotExist(err) {
		t.Fatalf("invalid preparation acquired resources: %v", err)
	}
}

func TestSDKIdentityFailureAndCancellationPrecedeResources(t *testing.T) {
	shape, _ := sdkFixture(t)
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	if err := os.WriteFile(kernel+".sha256", []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if r, err := StartRuntime(context.Background(), shape); r != nil || err == nil {
		t.Fatalf("invalid sidecar StartRuntime = %v/%v", r, err)
	}
	if err := os.Remove(kernel + ".sha256"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := StartRuntime(ctx, shape); r != nil || err == nil {
		t.Fatalf("canceled StartRuntime = %v/%v", r, err)
	}
	if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !os.IsNotExist(err) {
		t.Fatalf("failed startup acquired resources: %v", err)
	}
}

func TestProjectionUsesConfiguredSiblingOnly(t *testing.T) {
	shape, _ := sdkFixture(t)
	configured := strings.TrimPrefix(shape.Kernel, "file://")
	actual := sha256.Sum256([]byte("test kernel"))
	realPath := filepath.Join(shape.RuntimeRoot, "real-kernel")
	if err := os.Rename(configured, realPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, configured); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath+".sha256", []byte(strings.Repeat("b", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	projection, err := ResolvePortableProjection(shape.config())
	if err != nil || projection.KernelRef != testKernelRef("vmlinux", hex.EncodeToString(actual[:])) {
		t.Fatalf("followed target's sidecar: %+v, %v", projection, err)
	}
	if err := os.WriteFile(configured+".sha256", []byte(strings.Repeat("c", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	projection, err = ResolvePortableProjection(shape.config())
	if err != nil || projection.KernelRef != testKernelRef("vmlinux", strings.Repeat("c", 64)) {
		t.Fatalf("ignored configured sibling: %+v, %v", projection, err)
	}
}

func TestPreparedColdStartupDoesNotResolveAgain(t *testing.T) {
	shape, launch := sdkFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	identities, err := ResolvePortableProjection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	launch.PortableConfig, err = config.ProjectPortableCold(cfg, identities)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &preparedHostIdentity{kernel: shape.Kernel, bundle: shape.Bundle, projection: identities}
	// This deliberately violates deployment stability after preparation to
	// prove the private handoff itself does not open either identity path.
	for _, raw := range []string{shape.Kernel, shape.Bundle} {
		if err := os.Remove(strings.TrimPrefix(raw, "file://")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := start(ctx, SandboxSpec{Runtime: shape, Launch: launch}, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.hostProjection != identities {
		t.Fatalf("prepared identity changed: %+v vs %+v", r.hostProjection, identities)
	}
}

// Observe actual file I/O without mutable production hooks. Watches include
// open/close so distinct full-file scans cannot coalesce as adjacent IN_ACCESS
// events. The fixture kernel fits in one read, and the child does not load it.
type artifactAccessWatch struct {
	t  *testing.T
	fd int
}

func watchArtifactAccess(t *testing.T, path string) artifactAccessWatch {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, path, unix.IN_OPEN|unix.IN_ACCESS|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	return artifactAccessWatch{t, fd}
}
func (w artifactAccessWatch) count(mask uint32) int {
	w.t.Helper()
	nEvents := 0
	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(w.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			return nEvents
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			w.t.Fatal(err)
		}
		if n == 0 {
			return nEvents
		}
		for off := 0; off < n; {
			if n-off < unix.SizeofInotifyEvent {
				w.t.Fatal("truncated inotify event")
			}
			bits := binary.NativeEndian.Uint32(buf[off+4 : off+8])
			if bits&unix.IN_Q_OVERFLOW != 0 {
				w.t.Fatal("inotify overflow")
			}
			if bits&mask != 0 {
				nEvents++
			}
			off += unix.SizeofInotifyEvent + int(binary.NativeEndian.Uint32(buf[off+12:off+16]))
		}
	}
}
func TestSDKIdentityIOCompletesBeforeRuntimeReady(t *testing.T) {
	for _, sidecar := range []bool{false, true} {
		t.Run(fmtBool(sidecar), func(t *testing.T) {
			shape, launch := sdkFixture(t)
			kernel := strings.TrimPrefix(shape.Kernel, "file://")
			bundle := strings.TrimPrefix(shape.Bundle, "file://")
			sum := sha256.Sum256([]byte("test kernel"))
			expected := testKernelRef("vmlinux", hex.EncodeToString(sum[:]))
			var sidecarWatch artifactAccessWatch
			if sidecar {
				if err := os.WriteFile(kernel+".sha256", []byte(hex.EncodeToString(sum[:])+"  vmlinux\n"), 0600); err != nil {
					t.Fatal(err)
				}
				sidecarWatch = watchArtifactAccess(t, kernel+".sha256")
			}
			kernelWatch, bundleWatch := watchArtifactAccess(t, kernel), watchArtifactAccess(t, bundle)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := StartRuntime(ctx, shape)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.hostProjection.KernelRef != expected || r.hostProjection.RuntimeRef == "" || r.legacyUnknownKernel {
				t.Fatalf("incomplete public startup identity: %+v", r.hostProjection)
			}
			want := 1
			if sidecar {
				want = 0
			}
			if n := kernelWatch.count(unix.IN_ACCESS); n != want {
				t.Fatalf("kernel reads at RuntimeReady=%d, want %d", n, want)
			}
			if n := bundleWatch.count(unix.IN_OPEN); n != 1 {
				t.Fatalf("bundle opens at RuntimeReady=%d, want 1", n)
			}
			if sidecar {
				if n := sidecarWatch.count(unix.IN_OPEN); n != 1 {
					t.Fatalf("checksum opens=%d", n)
				}
			}
			if err := r.Launch(ctx, launch); err != nil {
				t.Fatal(err)
			}
			if n := kernelWatch.count(unix.IN_OPEN | unix.IN_ACCESS); n != 0 {
				t.Fatalf("Launch reopened/read kernel %d times", n)
			}
			if n := bundleWatch.count(unix.IN_OPEN | unix.IN_ACCESS); n != 0 {
				t.Fatalf("Launch reopened/read bundle %d times", n)
			}
			if sidecar {
				if n := sidecarWatch.count(unix.IN_OPEN | unix.IN_ACCESS); n != 0 {
					t.Fatalf("Launch reopened/read checksum %d times", n)
				}
			}
		})
	}
}
func fmtBool(v bool) string {
	if v {
		return "sidecar"
	}
	return "fallback"
}

func TestSDKStartPortableKeepsPublicIdentityPolicy(t *testing.T) {
	shape, launch := sdkFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	ids, err := ResolvePortableProjection(cfg)
	if err != nil {
		t.Fatal(err)
	}
	launch.PortableConfig, err = config.ProjectPortableCold(cfg, ids)
	if err != nil {
		t.Fatal(err)
	}
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	watch := watchArtifactAccess(t, kernel)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Start(ctx, SandboxSpec{Runtime: shape, Launch: launch})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n := watch.count(unix.IN_ACCESS); n != 1 {
		t.Fatalf("public PortableConfig Start kernel scans=%d, want 1", n)
	}
	if r.legacyUnknownKernel || r.hostProjection != ids {
		t.Fatalf("public Start used legacy policy: %+v", r.hostProjection)
	}
}

func TestSDKIdentityFailureBeforeStartup(t *testing.T) {
	for _, kind := range []string{"missing-kernel", "invalid-checksum", "invalid-bundle", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			shape, _ := sdkFixture(t)
			kernel := strings.TrimPrefix(shape.Kernel, "file://")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "missing-kernel":
				if err := os.Remove(kernel); err != nil {
					t.Fatal(err)
				}
			case "invalid-checksum":
				if err := os.WriteFile(kernel+".sha256", []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid-bundle":
				if err := os.WriteFile(strings.TrimPrefix(shape.Bundle, "file://"), []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			r, err := StartRuntime(ctx, shape)
			if err == nil || r != nil {
				if r != nil {
					r.Close()
				}
				t.Fatalf("StartRuntime=%v/%v", r, err)
			}
			if _, err := os.Stat(filepath.Join(shape.RuntimeRoot, shape.SandboxID)); !os.IsNotExist(err) {
				t.Fatalf("startup resources leaked: %v", err)
			}
		})
	}
}

func TestPreparedIdentityCannotChangeBindingsOrEscapeLegacy(t *testing.T) {
	shape, launch := sdkFixture(t)
	ids, err := ResolvePortableProjection(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedHostIdentity{kernel: shape.Kernel, bundle: shape.Bundle, projection: ids}
	for _, kind := range []string{"kernel", "bundle", "incomplete", "invalid-unknown"} {
		t.Run(kind, func(t *testing.T) {
			p := prepared
			switch kind {
			case "kernel":
				p.kernel += ".other"
			case "bundle":
				p.bundle += ".other"
			case "incomplete":
				p.projection.KernelRef = ""
			case "invalid-unknown":
				p.unknownKernel = true
			}
			r, e := startRuntime(context.Background(), shape, &p)
			if r != nil {
				r.Close()
			}
			if e == nil {
				t.Fatal("invalid preparation accepted")
			}
		})
	}
	unknown := prepared
	unknown.projection.KernelRef = ""
	unknown.unknownKernel = true
	if r, err := start(context.Background(), SandboxSpec{Runtime: shape, Launch: launch}, &unknown); err == nil {
		r.Close()
		t.Fatal("ordinary launch accepted legacy unknown identity")
	}
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	launch.PortableConfig, err = config.ProjectPortableCold(cfg, ids)
	if err != nil {
		t.Fatal(err)
	}
	launch.SourceBinding = &RunSourceBinding{SandboxRef: testKernelRef("source.sandbox", strings.Repeat("a", 64))}
	if r, err := start(context.Background(), SandboxSpec{Runtime: shape, Launch: launch}, &unknown); err == nil {
		r.Close()
		t.Fatal("nonlegacy portable input accepted unknown host identity")
	}
	// Even the private legacy state cannot generate a fresh C0 after startup.
	r, err := startRuntime(context.Background(), shape, &unknown)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	launch.PortableConfig = nil
	launch.SourceBinding = nil
	if err := r.Launch(context.Background(), launch); err == nil {
		t.Fatal("incomplete state generated ordinary C0")
	}
}

func TestKernelChecksumUsesConfiguredSiblingAndResolvesFresh(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(target, "vmlinux")
	if err := os.WriteFile(real, []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real+".sha256", []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, present, err := optionalKernelRef("file://" + alias); err != nil || present {
		t.Fatalf("searched symlink target sibling: %v/%v", present, err)
	}
	for _, digest := range []string{strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		if err := os.WriteFile(alias+".sha256", []byte(digest+"  alias\n"), 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := buildKernelRef("file://" + alias)
		if err != nil || ref != testKernelRef("alias", digest) {
			t.Fatalf("stale or incorrectly named identity: %s/%v", ref, err)
		}
	}
}

func TestKernelPreflightRetainsEffectiveReadability(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root effective readability for mode-000 fixture")
	}
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernel+".sha256", []byte("malformed metadata is irrelevant to restore"), 0600); err != nil {
		t.Fatal(err)
	}
	// Root can open the regular file. Restore's existing cheap preflight must
	// neither impose a new mode-bit policy nor start consulting the sidecar.
	if err := VerifyKernelArtifact("file://" + kernel); err != nil {
		t.Fatalf("changed cheap preflight: %v", err)
	}
}

func TestSandboxEChecksumProjectionMatchesFallback(t *testing.T) {
	shape, launch := sdkFixture(t)
	cfg, err := launch.config(shape.config())
	if err != nil {
		t.Fatal(err)
	}
	// NewPortableEROFS sets the artifact's root layout; workload defaults remain
	// identical between the two identity sources.
	plain, err := PrepareSandboxEConfig(context.Background(), cfg, []byte(`{"Cmd":["/bin/true"]}`), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	kernel := strings.TrimPrefix(shape.Kernel, "file://")
	sum := sha256.Sum256([]byte("test kernel"))
	if err := os.WriteFile(kernel+".sha256", []byte(hex.EncodeToString(sum[:])+"  vmlinux\n"), 0600); err != nil {
		t.Fatal(err)
	}
	watch := watchArtifactAccess(t, kernel)
	meta, err := PrepareSandboxEConfig(context.Background(), cfg, []byte(`{"Cmd":["/bin/true"]}`), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := config.MarshalPortableSandboxConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	after, err := config.MarshalPortableSandboxConfig(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("sidecar changed canonical Sandbox-E C0")
	}
	if n := watch.count(unix.IN_ACCESS); n != 0 {
		t.Fatalf("assembly read kernel despite sidecar: %d", n)
	}
}
