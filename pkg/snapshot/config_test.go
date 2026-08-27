package snapshot

import (
	"bytes"
	"strings"
	"testing"
)

const snapshotConfigTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestSnapshotConfigCanonicalStrictSchema(t *testing.T) {
	cfg := &Config{Version: 1, SandboxRef: "manifest://" + snapshotConfigTestKey,
		FromRefs: []string{"file://parent.snapshot@sha256:" + snapshotConfigTestKey}}
	a, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := MarshalConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("snapshot config encoding is not deterministic")
	}
	parsed, err := ParseConfig(a)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SandboxRef != cfg.SandboxRef || len(parsed.FromRefs) != 1 {
		t.Fatalf("parsed = %#v", parsed)
	}
	for _, raw := range []string{
		"version: 1\nsandbox_ref: manifest://" + snapshotConfigTestKey + "\nboot: {}\n",
		"version: 1\nversion: 1\nsandbox_ref: manifest://" + snapshotConfigTestKey + "\n",
		"version: 2\nsandbox_ref: manifest://" + snapshotConfigTestKey + "\n",
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid snapshot config:\n%s", raw)
		}
	}
}

func TestSnapshotConfigRejectsNonPortableAndDuplicateRefs(t *testing.T) {
	for _, cfg := range []*Config{
		{Version: 1, SandboxRef: "file:///tmp/root.sandbox@sha256:" + snapshotConfigTestKey},
		{Version: 1, SandboxRef: "manifest://" + snapshotConfigTestKey, FromRefs: []string{"self"}},
		{Version: 1, SandboxRef: "manifest://" + snapshotConfigTestKey,
			FromRefs: []string{"manifest://" + snapshotConfigTestKey, "manifest://" + snapshotConfigTestKey}},
	} {
		if err := cfg.Validate(); err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Fatalf("Validate(%#v) = %v", cfg, err)
		}
	}
}
