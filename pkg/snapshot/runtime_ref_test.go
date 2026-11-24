package snapshot

import (
	"strings"
	"testing"
)

func TestConfigRejectsRuntimeMetadataInContentGraph(t *testing.T) {
	base := "manifest://" + strings.Repeat("a", 64)
	annotated := base + "@runtime:runtime.bundle:digest:" + strings.Repeat("b", 64)
	for _, cfg := range []Config{
		{Version: 1, SandboxRef: annotated},
		{Version: 1, SandboxRef: base, FromRefs: []string{annotated}},
	} {
		if _, err := MarshalConfig(&cfg); err == nil {
			t.Fatalf("runtime metadata accepted in immutable content: %+v", cfg)
		}
	}
}
