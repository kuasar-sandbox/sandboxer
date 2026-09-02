package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestResolvePathIDDefaultsToSandboxID(t *testing.T) {
	got, err := ResolvePathID("logical-sandbox", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "logical-sandbox" {
		t.Fatalf("ResolvePathID default = %q, want logical-sandbox", got)
	}
}

func TestResolvePathIDUsesIndependentPathID(t *testing.T) {
	got, err := ResolvePathID("logical-sandbox", "c")
	if err != nil {
		t.Fatal(err)
	}
	if got != "c" {
		t.Fatalf("ResolvePathID explicit = %q, want c", got)
	}
}

func TestValidatePathIDRejectsUnsafeComponents(t *testing.T) {
	for _, pathID := range []string{"", ".", "..", "../escape", "nested/id", `nested\id`, "nul\x00byte"} {
		t.Run(pathID, func(t *testing.T) {
			if err := ValidatePathID(pathID); err == nil {
				t.Fatalf("ValidatePathID(%q) succeeded", pathID)
			}
		})
	}
	for _, pathID := range []string{"a", "phase_C-01", "logical-sandbox"} {
		if err := ValidatePathID(pathID); err != nil {
			t.Fatalf("ValidatePathID(%q): %v", pathID, err)
		}
	}
}

func TestRunRejectsUnsafePathIDBeforeDirectorySideEffects(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "run")
	_, err := Run(context.Background(), RunOptions{
		Cfg:         &config.SandboxConfig{},
		SandboxID:   "logical-sandbox",
		PathID:      "../escape",
		RuntimeRoot: runtimeRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "path id") {
		t.Fatalf("Run error = %v, want PathID rejection", err)
	}
	if _, statErr := os.Stat(runtimeRoot); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe PathID created runtime state: %v", statErr)
	}
}
