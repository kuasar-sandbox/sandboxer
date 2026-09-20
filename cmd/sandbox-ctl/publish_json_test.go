package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

func TestPublishJSONAliases(t *testing.T) {
	t.Setenv(config.ManifestConfigEnv, "")
	ctx := context.Background()
	dir, output := t.TempDir(), t.TempDir()
	cfg := testRunFromPortable(t)
	cfg.Boot.Root = config.PortableRootConfig{Base: "self"}
	runtime, err := config.MarshalPortableSandboxConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte{0x51}, 4096)
	e, err := sandboxfile.BuildSource(sparse.Dense(bytes.NewReader(body), uint64(len(body))), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	sink := snapshot.NewFileSink(dir, "cli", nil, false, nil)
	eRef, ePath, err := sink.AbsorbSandbox(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	scfg, _ := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: eRef})
	s, err := snapshotfile.BuildSource(sparse.Dense(bytes.NewReader(body), uint64(len(body))), []byte("{}"), []byte("{}"), scfg)
	if err != nil {
		t.Fatal(err)
	}
	_, sPath, err := sink.AbsorbSnapshot(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"publish", "upload-snapshot"} {
		for _, root := range []string{ePath, sPath} {
			for _, quiet := range []bool{false, true} {
				args := []string{"--to-ref-location", "result=file://" + output}
				if quiet {
					args = append(args, "--quiet")
				}
				rc, plain, stderr := captureInfoOutput(t, func() int { return publishArtifactCmd(command, append(args, root)) })
				if rc != 0 {
					t.Fatalf("plain: %s", stderr)
				}
				rc, stdout, stderr := captureInfoOutput(t, func() int { return publishArtifactCmd(command, append(append(args, "--json"), root)) })
				if rc != 0 || (quiet && stderr != "") {
					t.Fatalf("json: %d %s", rc, stderr)
				}
				role := artifact.RoleSandbox
				if root == sPath {
					role = artifact.RoleSnapshot
				}
				report, err := artifact.DecodePublishReport([]byte(stdout), role)
				if err != nil {
					t.Fatal(err)
				}
				final := report.SandboxRef
				if root == sPath {
					final = report.SnapshotRef
				}
				if plain != final+"\n" || strings.Contains(stdout, dir) || strings.Contains(stdout, output) {
					t.Fatalf("invalid output: %q %q", plain, stdout)
				}
				storage, _ := artifact.NewProcessStorage(nil)
				info, err := storage.Inspect(ctx, final, config.RefLocations{"result": output})
				storage.Close()
				if err != nil {
					t.Fatal(err)
				}
				if root == sPath && report.SandboxRef != info.Snapshot.SandboxRef {
					t.Fatal("wrong final E")
				}
				var fields map[string]json.RawMessage
				json.Unmarshal([]byte(stdout), &fields)
				want := 2
				if root == sPath {
					want = 3
				}
				if len(fields) != want {
					t.Fatalf("shape=%s", stdout)
				}
			}
		}
	}
	// A failed operation emits no successful JSON.
	rc, stdout, _ := captureInfoOutput(t, func() int {
		return publishCmd([]string{"--json", "--to-ref-location", "result=file://" + output, filepath.Join(dir, "missing.snapshot")})
	})
	if rc == 0 || stdout != "" {
		t.Fatalf("failed publish output=%q rc=%d", stdout, rc)
	}
	if _, err := os.Stat(sPath); err != nil {
		t.Fatal("publication removed source")
	}
}
