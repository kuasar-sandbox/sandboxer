package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func writeInfoSnapshot(t *testing.T, sink *snapshot.FileSink, memory, snapshotConfig []byte) string {
	t.Helper()
	logical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(memory), uint64(len(memory))),
		[]byte("{}"), []byte("{}"), snapshotConfig,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := sink.AbsorbSnapshot(context.Background(), logical)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInfoLocalCryptoPolicy(t *testing.T) {
	key := [32]byte{0x41, 0x42, 0x43}
	codec, _ := manifestcrypto.NewTarStreamCodec(key)
	memory := bytes.Repeat([]byte{0x29}, 4096)
	snapshotConfig := []byte("version: 1\nsandbox_ref: manifest://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	encryptedPath := writeInfoSnapshot(t, snapshot.NewFileSink(t.TempDir(), "encrypted", codec, true, nil), memory, snapshotConfig)
	plainPath := writeInfoSnapshot(t, snapshot.NewFileSink(t.TempDir(), "plain", nil, false, nil), memory, snapshotConfig)
	plainDigest := strings.TrimSuffix(filepath.Base(plainPath), filepath.Ext(plainPath))

	configPath := filepath.Join(t.TempDir(), "manifest.yaml")
	writeConfig := func(policy string) {
		body := fmt.Sprintf("manifest:\n  key: %s\ncrypto:\n  chunk: aes\n  manifest: aes\n  local: %s\n", hex.EncodeToString(key[:]), policy)
		if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("required")
	rc, stdout, stderr := captureInfoOutput(t, func() int {
		return infoCmd([]string{"--manifest-config", configPath, encryptedPath})
	})
	if rc != 0 || !strings.Contains(stdout, "sandbox_ref: manifest://") || stderr != "" {
		t.Fatalf("required encrypted rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
	if strings.Contains(stdout, plainDigest) || strings.Contains(stderr, plainDigest) {
		t.Fatal("info exposed the inner plaintext digest")
	}
	rc, _, stderr = captureInfoOutput(t, func() int {
		return infoCmd([]string{"--manifest-config", configPath, plainPath})
	})
	if rc == 0 || strings.Contains(stderr, plainDigest) {
		t.Fatalf("required plaintext rc=%d stderr=%q", rc, stderr)
	}
	writeConfig("auto")
	rc, stdout, stderr = captureInfoOutput(t, func() int {
		return infoCmd([]string{"--manifest-config", configPath, plainPath})
	})
	if rc != 0 || !strings.Contains(stdout, "sandbox_ref: manifest://") || stderr != "" {
		t.Fatalf("auto plaintext rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}

	literalDir := filepath.Join(t.TempDir(), "literal@location:not-a-ref")
	if err := os.Mkdir(literalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	literalPath := filepath.Join(literalDir, "backup.snapshot")
	if err := os.Rename(plainPath, literalPath); err != nil {
		t.Fatal(err)
	}
	rc, stdout, stderr = captureInfoOutput(t, func() int { return infoCmd([]string{literalPath}) })
	if rc != 0 || !strings.Contains(stdout, "sandbox_ref: manifest://") || stderr != "" {
		t.Fatalf("literal path rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
}

func TestInfoRejectsMalformedSnapshotCfgForRawAndJSON(t *testing.T) {
	malformed := []byte("resources: [")
	path := writeInfoSnapshot(t, snapshot.NewFileSink(t.TempDir(), "malformed", nil, false, nil),
		bytes.Repeat([]byte{0x17}, 4096), malformed)
	rc, stdout, stderr := captureInfoOutput(t, func() int { return infoCmd([]string{path}) })
	if rc == 0 || stdout != "" || stderr == "" {
		t.Fatalf("raw info accepted malformed snapshot.cfg: rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
	rc, _, stderr = captureInfoOutput(t, func() int { return infoCmd([]string{"--json", path}) })
	if rc == 0 || stderr == "" {
		t.Fatalf("JSON info accepted malformed snapshot.cfg: rc=%d stderr=%q", rc, stderr)
	}
}

func TestInfoRejectsExtraPositionalArgument(t *testing.T) {
	rc, stdout, stderr := captureInfoOutput(t, func() int {
		return infoCmd([]string{"first.snapshot", "second.snapshot"})
	})
	if rc != 2 || stdout != "" || !strings.Contains(stderr, "usage:") {
		t.Fatalf("info extra argument rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
}

func TestInfoJSONDerivesDiskGraphFromReferencedSandbox(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	portable := &config.PortableSandboxConfig{
		Version: config.PortableSandboxConfigVersion,
		Resources: config.PortableResourcesConfig{
			Capacity:    config.CapacityConfig{CPU: 2, Memory: "1GiB"},
			Allocatable: config.AllocatableConfig{CPU: 1, Memory: "768MiB"},
		},
		Boot: config.PortableBootConfig{
			Kernel:  "file://vmlinux@sha256:" + digest,
			Runtime: "file://sandbox-runtime.bundle@sha256:" + digest,
			Root:    config.PortableRootConfig{Base: "self"},
		},
		Launch:   config.PortableLaunchConfig{Workdir: "/", Restart: "never", CgroupControl: true},
		Metadata: map[string]string{"e2b.start_cmd": "touch /ready"},
	}
	portableRaw, err := config.MarshalPortableSandboxConfig(portable)
	if err != nil {
		t.Fatal(err)
	}
	eLogical, err := sandboxfile.BuildSource(
		sparse.Dense(bytes.NewReader(bytes.Repeat([]byte{0x31}, 4096)), 4096), nil, portableRaw,
	)
	if err != nil {
		t.Fatal(err)
	}
	eSink := snapshot.NewFileSink(dir, "info-e", nil, false, nil)
	eRef, ePath, err := eSink.AbsorbSandbox(context.Background(), eLogical)
	if err != nil {
		t.Fatal(err)
	}
	if err := eSink.CommitSandbox(context.Background(), eRef, ePath); err != nil {
		t.Fatal(err)
	}
	snapshotRaw, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: eRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	sPath := writeInfoSnapshot(t, snapshot.NewFileSink(dir, "info-s", nil, false, nil),
		bytes.Repeat([]byte{0x41}, 4096), snapshotRaw)

	rc, stdout, stderr := captureInfoOutput(t, func() int { return infoCmd([]string{"--json", sPath}) })
	if rc != 0 || stderr != "" {
		t.Fatalf("info JSON rc=%d stderr=%q", rc, stderr)
	}
	var got restore.SnapshotCfg
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decode info JSON: %v\n%s", err, stdout)
	}
	if got.Version != snapshot.SnapshotConfigVersion || got.SandboxRef != eRef {
		t.Fatalf("snapshot identity = version %d sandbox_ref %q", got.Version, got.SandboxRef)
	}
	if got.Boot.Root.Base != eRef || got.Resources.Capacity.Memory != "1GiB" ||
		!got.Launch.CgroupControl || got.Metadata["e2b.start_cmd"] != "touch /ready" {
		t.Fatalf("resolved S -> E info = %+v", got)
	}

	rc, stdout, stderr = captureInfoOutput(t, func() int { return infoCmd([]string{sPath}) })
	if rc != 0 || stderr != "" || !strings.Contains(stdout, "sandbox_ref: ") || strings.Contains(stdout, "boot:") {
		t.Fatalf("raw info is not the memory-only snapshot.cfg: rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
}

func captureInfoOutput(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	rc := fn()
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	stdout, readOutErr := io.ReadAll(outR)
	stderr, readErrErr := io.ReadAll(errR)
	_ = outR.Close()
	_ = errR.Close()
	if readOutErr != nil || readErrErr != nil {
		t.Fatalf("capture output: stdout=%v stderr=%v", readOutErr, readErrErr)
	}
	return rc, string(stdout), string(stderr)
}
