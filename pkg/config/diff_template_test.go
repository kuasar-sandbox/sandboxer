package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDiffTemplateReferenceSelection(t *testing.T) {
	id := strings.Repeat("a", 64)
	for _, tc := range []struct {
		raw  string
		kind DiffTemplateKind
	}{
		{"file:///empty.ext4", DiffTemplateRaw},
		{"file:///empty.bundle", DiffTemplateRaw},
		{"file://empty.ext4@location:seeds", DiffTemplateRaw},
		{"file:///empty.tar@digest:" + id, DiffTemplateTarstream},
		{"file://empty.tar@hmac:" + id + "@location:seeds", DiffTemplateTarstream},
		{"file://empty.bundle@manifest:" + id + "@location:seeds", DiffTemplateBundle},
		{"manifest://" + id, DiffTemplateManifest},
		{"mkfs://ext4?size=10GiB", DiffTemplateMkfs},
	} {
		r, err := ParseDiffTemplate(tc.raw)
		if err != nil || r.Kind != tc.kind {
			t.Errorf("%q: kind=%s err=%v", tc.raw, r.Kind, err)
		}
	}
	for _, raw := range []string{"", "/empty.ext4", "file://relative", "file://../escape@location:seeds", "file:///x@sha256:" + id, "file:///x@digest:" + strings.ToUpper(id), "file:///x@location:seeds@digest:" + id, "manifest://" + strings.ToUpper(id), "https://example.com/disk"} {
		if _, err := ParseDiffTemplate(raw); err == nil {
			t.Errorf("accepted invalid template %q", raw)
		}
	}
}

func TestMkfsSyntaxAndCanonicalCapacity(t *testing.T) {
	for _, suffix := range []string{"10G", "10GB", "10GiB", "10240MiB", "10737418240", "1.25GiB"} {
		r, err := ParseDiffTemplate("mkfs://ext4?size=" + suffix)
		if err != nil {
			t.Fatal(suffix, err)
		}
		want := int64(10 << 30)
		if suffix == "1.25GiB" {
			want = 5 << 28
		}
		if r.Size != want {
			t.Fatalf("%s: size=%d want=%d", suffix, r.Size, want)
		}
		canonical, err := NormalizeDiffTemplate(r.String())
		if err != nil || canonical != r.String() {
			t.Fatalf("normalization not idempotent: %s %v", canonical, err)
		}
	}
	for _, raw := range []string{"mkfs://ext4", "mkfs://ext4?size=", "mkfs://ext4?size=0", "mkfs://ext4?size=-1G", "mkfs://ext4?size=1G&size=1G", "mkfs://ext4?size=1G&uuid=x", "mkfs://xfs?size=1G", "mkfs://ext4/?size=1G", "mkfs://user@ext4?size=1G", "mkfs://ext4:1?size=1G", "mkfs://ext4?size=1G#", "mkfs:ext4?size=1G", "mkfs://ext4?size=1 G", "mkfs://ext4?size=%", "mkfs://ext4?size=9223372036854775808"} {
		if _, err := ParseDiffTemplate(raw); err == nil {
			t.Errorf("accepted invalid URI %q", raw)
		}
	}
	// An ignored template must not impose generator or target limits on an
	// existing disk; selected Source creation performs those checks instead.
	for _, raw := range []string{"mkfs://ext4?size=1", "mkfs://ext4?size=2TiB"} {
		if _, err := ParseDiffTemplate(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
}

func TestDiskV1CreationNormalizationAndIdentity(t *testing.T) {
	size := int64(10 << 30)
	short := DiskV1{Kind: "disk", Version: 1, Size: &size}
	normalized, err := short.NormalizeCreate()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.DiffTemplate != "mkfs://ext4?size=10737418240" || normalized.Size == nil || *normalized.Size != size {
		t.Fatalf("normalized=%+v", normalized)
	}
	if short.DiffTemplate != "" {
		t.Fatal("normalization modified input")
	}
	for _, raw := range []string{"mkfs://ext4?size=10G", "mkfs://ext4?size=10240MiB", normalized.DiffTemplate} {
		explicit := short
		explicit.DiffTemplate = raw
		if err := explicit.Validate(); err != nil {
			t.Fatal(err)
		}
		if explicit.Identity() != short.Identity() {
			t.Fatal("equivalent creation identity changed", raw)
		}
	}
	other := short
	larger := size * 2
	other.Size = &larger
	if other.Identity() == short.Identity() {
		t.Fatal("different generated sizes share identity")
	}
	id := strings.Repeat("a", 64)
	external := DiskV1{Kind: "disk", Version: 1, DiffTemplate: "file:///old.tar@digest:" + id, Size: &size}
	relocated := external
	relocated.DiffTemplate = "file://new.tar@digest:" + id + "@location:seeds"
	relocated.Size = &larger
	if external.Identity() != relocated.Identity() {
		t.Fatal("locator or capacity assertion changed content identity")
	}
	for _, invalid := range []DiskV1{
		{Size: &size},
		{Kind: "disk", Version: 1},
		{Kind: "disk", Version: 1, Size: &size, BaseFromRefs: []string{"manifest://" + id}},
		{Kind: "disk", Version: 1, Size: &size, DiffTemplate: "mkfs://ext4"},
		{Kind: "disk", Version: 1, Size: &size, Base: "missing"},
	} {
		if _, err := invalid.NormalizeCreate(); err == nil {
			t.Errorf("accepted invalid %+v", invalid)
		}
	}
}

func TestDiskV1WireNormalizesCreationWithoutRelaxingMarkers(t *testing.T) {
	var fromJSON, fromYAML DiskV1
	if err := json.Unmarshal([]byte(`{"kind":"disk","version":1,"size":10737418240}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte("kind: disk\nversion: 1\nsize: 10737418240\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromJSON.DiffTemplate != fromYAML.DiffTemplate || fromJSON.Identity() != fromYAML.Identity() {
		t.Fatal("wire formats differ")
	}
	for _, raw := range []string{`{"size":10737418240}`, `{"kind":"disk","size":10737418240}`, `{"kind":"disk","version":1,"size":"10GiB"}`, `{"kind":"disk","version":1,"size":4096}`, `{"kind":"disk","version":1,"size":10737418240,"size":10737418240}`} {
		if err := json.Unmarshal([]byte(raw), &fromJSON); err == nil {
			t.Errorf("accepted invalid %s", raw)
		}
	}
}
