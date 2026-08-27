package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestExportCommandRejectsInvalidModeCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "offline resume",
			args: []string{"--from", "image.erofs", "--config", "sandbox.yaml", "--output", t.TempDir(), "--resume"},
		},
		{
			name: "live config",
			args: []string{"--sandbox-id", "sid", "--config", "sandbox.yaml", "--output", t.TempDir()},
		},
		{
			name: "missing source",
			args: []string{"--output", t.TempDir()},
		},
		{
			name: "both outputs",
			args: []string{"--sandbox-id", "sid", "--output", t.TempDir(), "--upload"},
		},
		{
			name: "upload mode",
			args: []string{"--sandbox-id", "sid", "--upload", "--mode", "bundle"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if code := exportCmd(test.args); code != 2 {
				t.Fatalf("export exit = %d, want usage error 2", code)
			}
		})
	}
}

func TestOfflineExportRejectsNestedSandboxArtifact(t *testing.T) {
	ctx := context.Background()
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	pathDir := t.TempDir()
	_, path, err := snapshot.NewFileSink(pathDir, "nested", nil, false, nil).
		AbsorbSandbox(ctx, runFromLogical(t, runFromDirectPortable(t)))
	if err != nil {
		t.Fatal(err)
	}
	image, err := openFlattenedExportSource(ctx, path, storage, nil)
	if image != nil {
		_ = image.Close()
		t.Fatal("offline export accepted an existing Sandbox wrapper")
	}
	if err == nil || !strings.Contains(err.Error(), "flattened") {
		t.Fatalf("nested Sandbox error = %v", err)
	}
}
