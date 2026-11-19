package sandbox

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

func TestPrepareDiffReturnsInitializationPlanWithoutSideEffects(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "active.diff")
	template := filepath.Join(dir, "template.ext4")
	if err := os.WriteFile(template, []byte("template must not be copied during planning"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := PrepareDiffWithStorage(context.Background(), target, "file://"+template, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Existing || plan.Template == nil || plan.CreateSize != 0 {
		t.Fatalf("template plan=%#v", plan)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("planning created target: %v", err)
	}

	plan, err = PrepareDiffWithStorage(context.Background(), target, "", capacityBase{bytes.NewReader(make([]byte, 8*4096))}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Existing || plan.Template != nil || plan.CreateSize != 8*4096 {
		t.Fatalf("base plan=%#v", plan)
	}

	if err := os.WriteFile(target, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err = PrepareDiffWithStorage(context.Background(), target, "not-a-valid-template-uri", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Existing || plan.Template != nil || plan.CreateSize != 0 {
		t.Fatalf("existing plan=%#v", plan)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "existing" {
		t.Fatalf("planning modified existing target: %q err=%v", body, err)
	}
}

func TestPrepareDiffRejectsInvalidFreshSources(t *testing.T) {
	target := filepath.Join(t.TempDir(), "active.diff")
	if _, err := PrepareDiffWithStorage(context.Background(), target, "manifest://not-a-file", nil, nil, nil); err == nil {
		t.Fatal("manifest diff template was accepted")
	}
	if _, err := PrepareDiffWithStorage(context.Background(), target, "", nil, nil, nil); err == nil {
		t.Fatal("fresh diff without an ext4 source was accepted")
	}
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDiffWithStorage(context.Background(), target, "file:///unused-template", nil, nil, nil); err == nil {
		t.Fatal("existing empty diff was accepted")
	}
}

func TestDefaultDiffPathSeparatesPathIDFromSandboxID(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), "base")
	baseDir := DefaultBaseDir(baseRoot, "phase-b")
	if want := filepath.Join(baseRoot, "phase-b"); baseDir != want {
		t.Fatalf("base dir = %q, want %q", baseDir, want)
	}
	if got, want := DefaultDiffURIForBaseDir(baseDir, "logical-sandbox"),
		"file://"+filepath.Join(baseRoot, "phase-b", "logical-sandbox.overlay.diff"); got != want {
		t.Fatalf("default diff = %q, want %q", got, want)
	}
	if got, want := DefaultDiskDiffURI(baseDir, "logical-sandbox", "disk0"),
		"file://"+filepath.Join(baseRoot, "phase-b", "logical-sandbox.disk0.diff"); got != want {
		t.Fatalf("default data diff = %q, want %q", got, want)
	}
}

func TestPrepareDiffRejectsDanglingTargetAndKeepsExistingAlias(t *testing.T) {
	dir := t.TempDir()
	path, target := filepath.Join(dir, "alias.diff"), filepath.Join(dir, "target.diff")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDiffWithStorage(context.Background(), path, "mkfs://ext4?size=1MiB", nil, nil, nil); err == nil {
		t.Fatal("dangling target selected a generator")
	}
	if err := os.WriteFile(target, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareDiffWithStorage(context.Background(), path, "invalid-unused-template", nil, nil, nil)
	if err != nil || !plan.Existing || plan.Template != nil {
		t.Fatalf("existing alias: %+v %v", plan, err)
	}
}

func TestPrepareDiffLegacyRawCallerRetainsLazyCapacity(t *testing.T) {
	dir := t.TempDir()
	path, raw := filepath.Join(dir, "active.diff"), filepath.Join(dir, "raw.ext4")
	const size = 8192
	initial := bytes.Repeat([]byte{0x5a}, size)
	if err := os.WriteFile(raw, initial, 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareDiff(path, "file://"+raw, size/2)
	if err != nil || plan.Template == nil {
		t.Fatalf("legacy planning: %+v %v", plan, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plan created target: %v", err)
	}
	disk, err := diffstore.OpenDisk(path, nil, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	if capacity, err := diffstore.ExistingDiffCapacity(path); err != nil || capacity != size {
		t.Fatalf("raw capacity = %d: %v", capacity, err)
	}
	plan, err = PrepareDiff(path, "unavailable-unused-source", 0)
	if err != nil || !plan.Existing || plan.Template != nil {
		t.Fatalf("legacy reopen: %+v %v", plan, err)
	}
	if _, err := PrepareDiff(filepath.Join(dir, "new.diff"), "mkfs://ext4?size=1MiB", 0); err == nil {
		t.Fatal("legacy entry silently expanded its source capabilities")
	}
}

func TestDefaultDiffURIPreservesBaseRootContract(t *testing.T) {
	baseRoot := filepath.Join(t.TempDir(), "base")
	if got, want := DefaultDiffURI(baseRoot, "logical-sandbox"),
		"file://"+filepath.Join(baseRoot, "logical-sandbox", "logical-sandbox.overlay.diff"); got != want {
		t.Fatalf("default diff = %q, want %q", got, want)
	}
}
