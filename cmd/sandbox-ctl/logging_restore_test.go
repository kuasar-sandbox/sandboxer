package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
)

func TestRestoreDiagnosticUsesSelectedComponentWriter(t *testing.T) {
	var diagnostics strings.Builder
	root := filepath.Join(t.TempDir(), "untouched")
	cfg := &config.SandboxConfig{Restore: config.RestoreConfig{Prefetch: "invalid"}}
	rc, stderr := captureStderr(t, func() int {
		return runRestore(context.Background(), cfg, config.FieldPresence{}, nil,
			"manifest://deadbeef", "test", "", "/nonexistent/cloud-hypervisor",
			root, root, "", stdio.Defaults, 0, 0, nil, nil,
			nil, nil, nil, false, nil, &diagnostics)
	})
	if rc != 1 || !strings.Contains(diagnostics.String(), "restore.prefetch") {
		t.Fatalf("rc=%d diagnostics=%q", rc, diagnostics.String())
	}
	if stderr != "" {
		t.Fatalf("diagnostic duplicated to original stderr: %q", stderr)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("invalid restore touched runtime root: %v", err)
	}
}
