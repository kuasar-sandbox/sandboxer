package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestPublishRejectsConflictingInputAndTargetLocation(t *testing.T) {
	t.Setenv(config.ManifestConfigEnv, "")
	sourceDir := filepath.Join(t.TempDir(), "source")
	targetDir := filepath.Join(t.TempDir(), "target")

	rc, stderr := captureStderr(t, func() int {
		return publishCmd([]string{
			"--ref-location", "shared=file://" + sourceDir,
			"--to-ref-location", "shared=file://" + targetDir,
			"file://root.sandbox@location:shared",
		})
	})
	if rc != 2 || !strings.Contains(stderr, "conflicts with input ref location") {
		t.Fatalf("publish conflict rc=%d stderr=%q", rc, stderr)
	}
	if _, err := os.Stat(targetDir); !os.IsNotExist(err) {
		t.Fatalf("conflicting publish target was touched: stat error=%v", err)
	}
}
