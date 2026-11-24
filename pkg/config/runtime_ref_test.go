package config

import (
	"strings"
	"testing"
)

func TestRuntimeAnnotationOnlyAppliesToSnapshotInput(t *testing.T) {
	annotation := "@runtime:runtime.bundle:digest:" + strings.Repeat("a", 64)
	if err := requireRuntimeFileAbs("boot.runtime", "file:///runtime.bundle"+annotation); err == nil {
		t.Fatal("boot.runtime accepted recursive snapshot declaration")
	}
	if _, err := ParseDiffTemplate("manifest://" + strings.Repeat("b", 64) + annotation); err == nil {
		t.Fatal("diff template accepted snapshot declaration")
	}
	if err := validatePortableRef("boot.runtime", "file://runtime.bundle@digest:"+strings.Repeat("b", 64)+annotation, false); err == nil {
		t.Fatal("portable runtime accepted recursive snapshot declaration")
	}
}
