package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishAliasesRejectOverlappingRulesBeforeOpeningAnything(t *testing.T) {
	a, b := "manifest://"+strings.Repeat("a", 64), "manifest://"+strings.Repeat("b", 64)
	for _, alias := range []string{"publish", "upload-snapshot"} {
		for _, skip := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/skip=%t", alias, skip), func(t *testing.T) {
				dir := t.TempDir()
				target := filepath.Join(dir, "uncreated")
				rc, stderr := captureStderr(t, func() int {
					return publishArtifactCmd(alias, []string{
						"--manifest-config", filepath.Join(dir, "missing-config"),
						"--to-ref-location", "result=file://" + target,
						"--replace-ref", a + "=" + b, "--reduce-ref", a + "=" + b,
						fmt.Sprintf("--skip-verify-ref=%t", skip), filepath.Join(dir, "missing-source"),
					})
				})
				if rc != 2 || !strings.Contains(stderr, "overlap") {
					t.Fatalf("rc=%d stderr=%q", rc, stderr)
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("invalid flags touched target: %v", err)
				}
			})
		}
	}
}
