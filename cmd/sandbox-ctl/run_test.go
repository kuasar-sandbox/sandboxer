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
	"github.com/kuasar-sandbox/sandboxer/internal/tartransition"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/restore"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
	"gopkg.in/yaml.v3"
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

func callRunRestoreForValidation(t *testing.T, cfg *config.SandboxConfig, manifestCfg *config.ManifestConfig, runRoot string) (int, string) {
	t.Helper()
	return captureStderr(t, func() int {
		return runRestore(
			context.Background(), cfg, manifestCfg, "manifest://deadbeef",
			"test-sandbox", "/nonexistent/cloud-hypervisor",
			runRoot, filepath.Join(t.TempDir(), "base"), "", stdio.Defaults, 0, 0, nil, nil,
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
	ref := "file://" + snapshotPath + "@sha256:" + strings.Repeat("f", 64)
	rc, stderr := captureStderr(t, func() int {
		return runRestore(
			context.Background(), &config.SandboxConfig{}, nil, ref,
			"test-sandbox", "/nonexistent/cloud-hypervisor", runRoot,
			filepath.Join(t.TempDir(), "base"), "", stdio.Defaults, 0, 0, nil, nil,
		)
	})
	if rc != 1 || !strings.Contains(stderr, "sha256 marker mismatch") {
		t.Fatalf("runRestore = %d, stderr %q; want digest mismatch", rc, stderr)
	}
	if _, err := os.Stat(runRoot); !os.IsNotExist(err) {
		t.Fatalf("runtime root was touched before digest validation: %v", err)
	}
}

func writeRunRestoreSnapshot(t *testing.T) string {
	t.Helper()
	cfg := &restore.SnapshotCfg{}
	cfg.Resources.Capacity.Memory = "4KiB"
	configBody, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	zipBody, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  []byte("{}"),
		"state.json":   []byte("{}"),
		"snapshot.cfg": configBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := append(make([]byte, 4096), zipBody...)
	dir := t.TempDir()
	tmp, err := os.CreateTemp(dir, "snapshot-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := tartransition.WriteTo(context.Background(), tmp, "snapshot", sparse.Dense(bytes.NewReader(payload), uint64(len(payload))))
	if err != nil {
		tmp.Close()
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")+".snapshot")
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
	return path
}
