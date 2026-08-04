package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestInfoLocalCryptoPolicy(t *testing.T) {
	key := [32]byte{0x41, 0x42, 0x43}
	codec, _ := manifestcrypto.NewTarStreamCodec(key)
	zipBody, err := snapshot.BuildZIP(map[string][]byte{
		"config.json":  {},
		"state.json":   {},
		"snapshot.cfg": []byte("resources:\n  capacity:\n    cpu: 1\n    memory: 4KiB\nboot: {}\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := bytes.Repeat([]byte{0x29}, 4096)
	_, encryptedPath, err := snapshot.NewFileSink(t.TempDir(), "encrypted", codec, true, nil).AbsorbBundle(context.Background(), bytes.NewReader(memory), nil, bytes.NewReader(zipBody))
	if err != nil {
		t.Fatal(err)
	}
	_, plainPath, err := snapshot.NewFileSink(t.TempDir(), "plain", nil, false, nil).AbsorbBundle(context.Background(), bytes.NewReader(memory), nil, bytes.NewReader(zipBody))
	if err != nil {
		t.Fatal(err)
	}
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
	if rc != 0 || !strings.Contains(stdout, "memory: 4KiB") || stderr != "" {
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
	if rc != 0 || !strings.Contains(stdout, "memory: 4KiB") || stderr != "" {
		t.Fatalf("auto plaintext rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}

	literalDir := filepath.Join(t.TempDir(), "literal@location:not-a-ref")
	if err := os.Mkdir(literalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	literalPath := filepath.Join(literalDir, filepath.Base(plainPath))
	if err := os.Rename(plainPath, literalPath); err != nil {
		t.Fatal(err)
	}
	rc, stdout, stderr = captureInfoOutput(t, func() int { return infoCmd([]string{literalPath}) })
	if rc != 0 || !strings.Contains(stdout, "memory: 4KiB") || stderr != "" {
		t.Fatalf("literal path rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
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
