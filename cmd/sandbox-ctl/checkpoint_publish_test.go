package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestPublishCheckpointCleanupFailureReturns79WithUsableJSONArtifact(t *testing.T) {
	usePlaintextRefConfig(t)
	ctx := context.Background()
	directory, output := t.TempDir(), t.TempDir()
	owner, err := diffstore.LockDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	// No owner route is published: publication may pin the live source, while
	// opt-in cleanup must fail instead of bypassing its directory owner.
	portable := testRunFromPortable(t)
	portable.Boot.Root = config.PortableRootConfig{Base: "self"}
	portable, err = portable.Checkpointed("")
	if err != nil {
		t.Fatal(err)
	}
	store, err := diffstore.Create(filepath.Join(directory, "diffs", "root.diff"), diffstore.Options{DiffID: "root", BaseID: artifact.DiffCheckpointBaseID(portable.Boot.Root), LogicalSize: 8192})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	want := bytes.Repeat([]byte{0x61}, 8192)
	if _, err := store.WriteAt(want, 0); err != nil {
		t.Fatal(err)
	}
	if err := store.Checkpoint(ctx, "cp"); err != nil {
		t.Fatal(err)
	}
	cp := &sandboxfile.Checkpoint{Version: 1, CheckpointID: "cp", Root: sandboxfile.CheckpointDisk{DiffID: "root"}, Disks: []sandboxfile.CheckpointNamedDisk{}}
	source, err := sandboxfile.BuildCheckpointSource(ctx, portable, cp)
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := snapshot.NewFileSink(directory, "cli", nil, false, nil).AbsorbSandbox(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	rc, stdout, stderr := captureInfoOutput(t, func() int {
		return publishCmd([]string{"--delete-checkpoint", "--json", "--to-ref-location", "result=file://" + output, path})
	})
	if rc != 79 || !strings.Contains(stderr, "publication succeeded; checkpoint cleanup failed") {
		t.Fatalf("rc=%d stdout=%q stderr=%q", rc, stdout, stderr)
	}
	report, err := artifact.DecodePublishReport([]byte(stdout), artifact.RoleSandbox)
	if err != nil {
		t.Fatalf("cleanup failure lost successful JSON result: %v", err)
	}
	ref, err := manifest.ParseRef(report.SandboxRef)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := artifact.NewProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	stream, err := storage.OpenFileWithLocations(ctx, filepath.Join(output, ref.Path), ref, config.RefLocations{"result": output})
	if err != nil {
		t.Fatal(err)
	}
	root, err := sandboxfile.Open(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if root.Checkpoint != nil || root.Portable.IsCheckpoint() {
		t.Fatal("publication retained local checkpoint layout")
	}
	got := make([]byte, len(want))
	if _, err := root.Payload.ReadAt(ctx, got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("successful published artifact lost source payload")
	}
	ids, err := store.ListCheckpoints(ctx)
	if err != nil || len(ids) != 1 || ids[0] != "cp" {
		t.Fatalf("failed cleanup changed source versions: %v %v", ids, err)
	}
	view, err := store.Export(ctx, "cp")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if _, err := view.ReadAt(ctx, got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("failed cleanup changed source bytes")
	}
}

func TestCheckpointCaptureExitCodes(t *testing.T) {
	for _, command := range []string{"snapshot", "export"} {
		for _, failure := range []string{"unsupported", "older-runtime", "io"} {
			t.Run(command+"/"+failure, func(t *testing.T) {
				directory, err := os.MkdirTemp("", "checkpoint-cli-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(directory)
				run := filepath.Join(directory, "sid")
				if err := os.Mkdir(run, 0700); err != nil {
					t.Fatal(err)
				}
				listener, err := net.Listen("unix", filepath.Join(run, "ctl.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done := make(chan error, 1)
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					var req ctl.Request
					if err := ctl.ReadMessage(conn, &req); err != nil {
						done <- err
						return
					}
					resp := ctl.Response{Type: ctl.TypeError, Msg: "checkpoint storage I/O failure"}
					switch failure {
					case "unsupported":
						resp.Code = ctl.CheckpointUnsupportedCode
					case "older-runtime":
						resp.Msg = "unknown type: " + req.Type
					}
					done <- ctl.WriteMessage(conn, resp)
				}()
				rc, stdout, stderr := captureInfoOutput(t, func() int {
					args := []string{"--sandbox-id", "sid", "--run-root", directory, "--checkpoint", "cp", "--json"}
					if command == "snapshot" {
						return snapshotCmd(args)
					}
					return exportCmd(args)
				})
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				want := 78
				if failure == "io" {
					want = 1
				}
				if rc != want || stdout != "" || stderr == "" {
					t.Fatalf("rc=%d want=%d stdout=%q stderr=%q", rc, want, stdout, stderr)
				}
			})
		}
	}
}
