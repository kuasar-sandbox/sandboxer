package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
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
			name: "live manifest config",
			args: []string{"--sandbox-id", "sid", "--manifest-config", "manifest.yaml", "--output", t.TempDir()},
		},
		{
			name: "live ref location",
			args: []string{"--sandbox-id", "sid", "--ref-location", "release=file:///srv/artifacts", "--output", t.TempDir()},
		},
		{
			name: "negative timeout",
			args: []string{"--sandbox-id", "sid", "--timeout", "-1", "--output", t.TempDir()},
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
		{
			name: "positional argument",
			args: []string{"--sandbox-id", "sid", "--output", t.TempDir(), "extra"},
		},
		{
			name: "unsafe sandbox id",
			args: []string{"--sandbox-id", "../sid", "--output", t.TempDir()},
		},
		{
			name: "unsafe path id",
			args: []string{"--path-id", "../phase", "--output", t.TempDir()},
		},
		{
			name: "offline path id",
			args: []string{"--from", "image.erofs", "--config", "sandbox.yaml", "--path-id", "phase", "--output", t.TempDir()},
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

func TestExportCommandUsesPathIDWithoutSandboxIDAndWithPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		sandboxID string
	}{
		{name: "path id only"},
		{name: "path id takes precedence", sandboxID: "logical-sandbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runRoot, err := os.MkdirTemp("", "pathid-export-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
			pathID := "phase-a"
			runDir := filepath.Join(runRoot, pathID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(runDir, "ctl.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				var req ctl.Request
				if err := ctl.ReadMessage(conn, &req); err != nil {
					serverDone <- err
					return
				}
				serverDone <- ctl.WriteMessage(conn, &ctl.Response{Type: ctl.TypeExportDone})
			}()

			args := []string{"--path-id", pathID, "--run-root", runRoot, "--output", filepath.Join(t.TempDir(), "out")}
			if test.sandboxID != "" {
				args = append(args, "--sandbox-id", test.sandboxID)
			}
			if code := exportCmd(args); code != 0 {
				t.Fatalf("exportCmd exit=%d", code)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareArtifactOutputDirRejectsSymlink(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "output")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareArtifactOutputDir(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("prepareArtifactOutputDir error = %v, want symlink rejection", err)
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
