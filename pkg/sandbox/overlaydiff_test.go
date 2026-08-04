package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareDiffReturnsInitializationPlanWithoutSideEffects(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "active.diff")
	template := filepath.Join(dir, "template.ext4")
	if err := os.WriteFile(template, []byte("template must not be copied during planning"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := PrepareDiff(target, "file://"+template, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Existing || plan.TemplatePath != template || plan.CreateSize != 0 {
		t.Fatalf("template plan=%#v", plan)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("planning created target: %v", err)
	}

	plan, err = PrepareDiff(target, "", 8*4096, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Existing || plan.TemplatePath != "" || plan.CreateSize != 8*4096 {
		t.Fatalf("base plan=%#v", plan)
	}

	if err := os.WriteFile(target, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err = PrepareDiff(target, "not-a-valid-template-uri", 8*4096, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Existing || plan.TemplatePath != "" || plan.CreateSize != 0 {
		t.Fatalf("existing plan=%#v", plan)
	}
	if body, err := os.ReadFile(target); err != nil || string(body) != "existing" {
		t.Fatalf("planning modified existing target: %q err=%v", body, err)
	}
}

func TestPrepareDiffRejectsInvalidFreshSources(t *testing.T) {
	target := filepath.Join(t.TempDir(), "active.diff")
	if _, err := PrepareDiff(target, "manifest://not-a-file", 0, 1<<30); err == nil {
		t.Fatal("manifest diff template was accepted")
	}
	if _, err := PrepareDiff(target, "", 0, 1<<30); err == nil {
		t.Fatal("fresh diff without an ext4 source was accepted")
	}
}
