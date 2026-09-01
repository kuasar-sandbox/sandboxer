package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
)

func captureStderr(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = f
	defer func() {
		os.Stderr = old
		f.Close()
	}()

	rc := fn()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return rc, string(body)
}

func TestRunCmdRejectsUnsafeSandboxID(t *testing.T) {
	rc, stderr := captureStderr(t, func() int {
		return runCmd([]string{"--sandbox-id", "../escape", "--config", "missing.yaml"})
	})
	if rc != 2 || !strings.Contains(stderr, "one non-empty path component") {
		t.Fatalf("runCmd rc=%d stderr=%q", rc, stderr)
	}
}

func TestRunCmdRejectsPositionalArguments(t *testing.T) {
	rc, stderr := captureStderr(t, func() int {
		return runCmd([]string{"--config", "missing.yaml", "extra"})
	})
	if rc != 2 || !strings.Contains(stderr, "unexpected positional arguments") {
		t.Fatalf("runCmd rc=%d stderr=%q", rc, stderr)
	}
}

func callRunRestoreForValidation(t *testing.T, cfg *config.SandboxConfig, manifestCfg *config.ManifestConfig, runRoot string) (int, string) {
	t.Helper()
	return captureStderr(t, func() int {
		return runRestore(
			context.Background(), cfg, config.FieldPresence{}, manifestCfg, "manifest://deadbeef",
			"test-sandbox", "/nonexistent/cloud-hypervisor",
			runRoot, filepath.Join(t.TempDir(), "base"), "", stdio.Defaults, 0, 0, nil, nil,
			nil, nil, nil, false, nil,
		)
	})
}

func TestRunRestoreRejectsInvalidPrefetchBeforeMissingManifestConfig(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "run")
	cfg := &config.SandboxConfig{Restore: config.RestoreConfig{Prefetch: "disk"}}

	rc, stderr := callRunRestoreForValidation(t, cfg, nil, runRoot)
	if rc != 1 {
		t.Fatalf("runRestore return code = %d, want config error 1", rc)
	}
	if !strings.Contains(stderr, "restore.prefetch") {
		t.Fatalf("stderr = %q, want restore.prefetch error", stderr)
	}
	if strings.Contains(stderr, "requires --manifest-config") {
		t.Fatalf("stderr = %q, manifest-config check ran before prefetch validation", stderr)
	}
	if _, err := os.Stat(runRoot); !os.IsNotExist(err) {
		t.Fatalf("runtime root was touched for invalid prefetch: stat error = %v", err)
	}
}

func TestRunRestoreRejectsInvalidPrefetchBeforeRemoteDial(t *testing.T) {
	runRoot := filepath.Join(t.TempDir(), "run")
	cfg := &config.SandboxConfig{Restore: config.RestoreConfig{Prefetch: "disk"}}
	manifestCfg := &config.ManifestConfig{}
	manifestCfg.Crypto.Chunk = "aes"
	manifestCfg.Crypto.Manifest = "aes"
	manifestCfg.Cache.Endpoint = "unix://" + filepath.Join(t.TempDir(), "missing-cache.sock")

	rc, stderr := callRunRestoreForValidation(t, cfg, manifestCfg, runRoot)
	if rc != 1 {
		t.Fatalf("runRestore return code = %d, want config error 1", rc)
	}
	if !strings.Contains(stderr, "restore.prefetch") {
		t.Fatalf("stderr = %q, want restore.prefetch error before remote dial", stderr)
	}
	if strings.Contains(stderr, "dial cache") {
		t.Fatalf("stderr = %q, remote dial ran before prefetch validation", stderr)
	}
	if _, err := os.Stat(runRoot); !os.IsNotExist(err) {
		t.Fatalf("runtime root was touched for invalid prefetch: stat error = %v", err)
	}
}

func TestRunRestoreChecksDigestOnUnlocatedFileRef(t *testing.T) {
	snapshotPath := writeRunRestoreSnapshot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	ref := "file://" + snapshotPath + "@digest:" + strings.Repeat("f", 64)
	rc, stderr := captureStderr(t, func() int {
		return runRestore(
			context.Background(), &config.SandboxConfig{}, config.FieldPresence{}, nil, ref,
			"test-sandbox", "/nonexistent/cloud-hypervisor", runRoot,
			filepath.Join(t.TempDir(), "base"), "", stdio.Defaults, 0, 0, nil, nil,
			nil, nil, nil, false, nil,
		)
	})
	if rc != 1 || !strings.Contains(stderr, "digest mismatch") {
		t.Fatalf("runRestore = %d, stderr %q; want digest mismatch", rc, stderr)
	}
	if _, err := os.Stat(runRoot); !os.IsNotExist(err) {
		t.Fatalf("runtime root was touched before digest validation: %v", err)
	}
}

func writeRunRestoreSnapshot(t *testing.T) string {
	t.Helper()
	configBody, err := snapshot.MarshalConfig(&snapshot.Config{
		Version: snapshot.SnapshotConfigVersion, SandboxRef: "manifest://" + strings.Repeat("0", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	logical, err := snapshotfile.BuildSource(
		sparse.Dense(bytes.NewReader(make([]byte, 4096)), 4096),
		[]byte("{}"), []byte("{}"), configBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, "snapshot-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := tarstream.WriteTo(context.Background(), tmp, "snapshot", logical)
	if err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, digest+".snapshot")
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	return path
}
