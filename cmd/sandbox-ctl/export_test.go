package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/image"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandbox"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestExportCommandRejectsInvalidModeCombinations(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "assembly resume",
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
			name: "assembly path id",
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

func TestImageAssemblyRejectsNestedSandboxArtifact(t *testing.T) {
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
	image, err := sandbox.OpenFlattenedImage(ctx, path, storage, nil)
	if image != nil {
		_ = image.Close()
		t.Fatal("image assembly accepted an existing Sandbox wrapper")
	}
	if err == nil || !strings.Contains(err.Error(), "flattened") {
		t.Fatalf("nested Sandbox error = %v", err)
	}
}

func TestImageAssemblyCLIAndPackageAPIProduceSameLogicalSandboxE(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	kernelPath := filepath.Join(work, "vmlinux")
	if err := os.WriteFile(kernelPath, []byte("kernel fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(work, "sandbox-runtime.bundle")
	writeAssemblyRuntimeBundle(t, runtimePath)

	imagePath := filepath.Join(work, "flattened.img")
	payload := make([]byte, 4096)
	binary.LittleEndian.PutUint32(payload[1024:1028], 0xE0F5E1E2)
	payload[1024+12] = 12
	binary.LittleEndian.PutUint32(payload[1024+36:1024+40], 1)
	if err := os.WriteFile(imagePath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := image.AppendConfigZip(imagePath, &image.RuntimeConfig{
		Cmd: []string{"/bin/app", "serve"}, Env: []string{"IMAGE=yes"}, WorkingDir: "/srv",
	}); err != nil {
		t.Fatal(err)
	}
	flattened, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	imageSource, err := sparse.NewSource(bytes.NewReader(flattened), uint64(len(flattened)), nil)
	if err != nil {
		t.Fatal(err)
	}
	imageDir := filepath.Join(work, "image")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imageRef, imageCarrier, err := snapshot.NewFileSink(imageDir, "image", nil, false, nil).
		AbsorbImageSource(ctx, imageSource)
	if err != nil {
		t.Fatal(err)
	}
	parsedImageRef, err := manifest.ParseRef(imageRef)
	if err != nil {
		t.Fatal(err)
	}
	parsedImageRef.Path = imageCarrier
	imageRef = parsedImageRef.String()

	configPath := filepath.Join(work, "sandbox.yaml")
	configBytes := []byte(fmt.Sprintf(`resources:
  capacity: {cpu: 1, memory: 1GiB}
  allocatable: {cpu: 1, memory: 1GiB}
boot:
  kernel: file://%s
  runtime: file://%s
  root:
    base: manifest://%s
    overlay:
      diff_template: file:///var/lib/sandbox/assembly.ext4
launch:
  restart: never
`, kernelPath, runtimePath, strings.Repeat("b", 64)))
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(work, "output")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code := assembleSandboxEExport(imageRef, configPath, "cli", outputDir, false, ctl.SnapshotModeLocal, "", nil, 0, false); code != 0 {
		t.Fatalf("assembleSandboxEExport exit = %d", code)
	}

	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	directImage, err := sandbox.OpenFlattenedImage(ctx, imageRef, storage, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer directImage.Close()
	directConfig, err := config.LoadMerged([]string{configPath})
	if err != nil {
		t.Fatal(err)
	}
	opener := sandbox.FileStreamOpener(func(ctx context.Context, path string, ref manifest.Ref) (fetch.Stream, error) {
		return storage.OpenFileWithLocations(ctx, path, ref, nil)
	})
	portable, err := sandbox.PrepareSandboxEConfig(ctx, directConfig, directImage.ImageConfig, nil, nil, false, opener)
	if err != nil {
		t.Fatal(err)
	}
	directLogical, err := sandbox.AssembleSandboxE(ctx, directImage, portable)
	if err != nil {
		t.Fatal(err)
	}

	cliAlias := filepath.Join(outputDir, "cli.sandbox")
	cliRef := manifest.Ref{Scheme: manifest.RefSchemeFile, Path: cliAlias}
	cliLogical, err := storage.OpenFile(ctx, cliAlias, cliRef)
	if err != nil {
		t.Fatal(err)
	}
	got := readAssemblyLogical(t, cliLogical)
	if err := cliLogical.Close(); err != nil {
		t.Fatal(err)
	}
	want := readAssemblyLogical(t, directLogical)
	if !bytes.Equal(got, want) {
		t.Fatalf("CLI logical Sandbox E differs from package API: got=%d bytes want=%d", len(got), len(want))
	}

	strictLogical, err := storage.OpenFile(ctx, cliAlias, cliRef)
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, strictLogical)
	if err != nil {
		t.Fatal(err)
	}
	if root.Portable.Boot.Root.Base != "self" || root.Portable.Boot.Root.Overlay == nil || root.Portable.Launch.Exec != "/bin/app" {
		_ = root.Close()
		t.Fatalf("CLI assembled Sandbox E = %+v", root.Portable)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeAssemblyRuntimeBundle(t *testing.T, path string) {
	t.Helper()
	var footer bytes.Buffer
	writer := zip.NewWriter(&footer)
	header := &zip.FileHeader{
		Name:     tarstream.DigestMarkerPrefix + strings.Repeat("a", 64),
		Method:   zip.Store,
		Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := writer.CreateHeader(header); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2<<20)
	copy(body[len(body)-footer.Len():], footer.Bytes())
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAssemblyLogical(t *testing.T, source sparse.Source) []byte {
	t.Helper()
	body := make([]byte, source.Size())
	for offset := uint64(0); offset < source.Size(); {
		length := min(uint64(64<<10), source.Size()-offset)
		n, err := source.ReadAt(context.Background(), body[offset:offset+length], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if n != int(length) {
			t.Fatalf("logical source short read at %d: got=%d want=%d", offset, n, length)
		}
		offset += length
	}
	return body
}
