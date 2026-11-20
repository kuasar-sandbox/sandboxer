package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
)

func TestCheckpointWorkingSetRefLimitBeforeHolderPreparation(t *testing.T) {
	opts := checkpointMemoryRefLimitOptions(t, 1, snapshot.MaxMemoryFromRefs)
	opts.EnableCheckpoint = true
	opts.runtime = &Runtime{}
	working := false
	req := ctl.Request{CheckpointID: "next", MergeRef: &working}
	runRoot := t.TempDir()
	runDir := filepath.Join(runRoot, "sid")
	before, err := os.ReadDir(opts.checkpointDir)
	if err != nil {
		t.Fatal(err)
	}
	// A rejected graph must not touch the live memfd or prepare a holder,
	// capture record, resource retention, or runtime namespace.
	defer func() {
		if caught := recover(); caught != nil {
			t.Fatalf("reference limit reached live-memory preparation: %v", caught)
		}
	}()
	save, err := prepareCheckpointMemorySave(context.Background(), req, opts, nil, runDir)
	if save != nil || err == nil || !strings.Contains(err.Error(), "from_refs exceeds 64 entries") {
		t.Fatalf("working-set history overflow was not rejected: save=%v err=%v", save, err)
	}
	after, err := os.ReadDir(opts.checkpointDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected capture changed the checkpoint directory")
	}
	entries, err := os.ReadDir(runRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected capture prepared a runtime namespace: %v %v", entries, err)
	}
}

func TestCheckpointMemoryRefBudget(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		local, external       int
		working, wantOverflow bool
	}{
		{name: "working-history-63", local: 1, external: 63, working: true},
		{name: "working-history-64", local: 1, external: 64, working: true, wantOverflow: true},
		{name: "working-multiple-history-63", local: 2, external: 63, working: true},
		{name: "dirty-history-64", local: 1, external: 64},
		{name: "working-external-64", external: 64, working: true},
		{name: "dirty-external-64", external: 64},
		{name: "working-external-65", external: 65, working: true, wantOverflow: true},
		{name: "cold-working", working: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := checkpointMemoryRefLimitOptions(t, tc.local, tc.external)
			parents, external, err := checkpointMemoryParents(context.Background(), opts, tc.working)
			if tc.wantOverflow {
				if err == nil || !strings.Contains(err.Error(), "from_refs exceeds 64 entries") {
					t.Fatalf("expected reference overflow, got %v", err)
				}
				return
			}
			if err != nil || len(parents) != tc.local || len(external) != tc.external {
				t.Fatalf("unexpected plan: parents=%v external=%v err=%v", parents, external, err)
			}
			for i, ref := range external {
				if want := fmt.Sprintf("manifest://%064x", i+1); ref != want {
					t.Fatalf("external reference %d changed: %s, want %s", i, ref, want)
				}
			}
		})
	}
}

func TestCheckpointMemoryRefPreflightOnlyForFastCapture(t *testing.T) {
	for _, tc := range []struct {
		name   string
		adjust func(*RunOptions, *ctl.Request)
	}{
		{name: "checkpoint-disabled", adjust: func(o *RunOptions, _ *ctl.Request) { o.EnableCheckpoint = false }},
		{name: "unnamed-capture", adjust: func(_ *RunOptions, r *ctl.Request) { r.CheckpointID = "" }},
		{name: "resume-after", adjust: func(_ *RunOptions, r *ctl.Request) { r.ResumeAfter = true }},
		{name: "async-disabled", adjust: func(o *RunOptions, _ *ctl.Request) { o.memoryAsyncDisabled = true }},
		{name: "no-runtime-owner", adjust: func(o *RunOptions, _ *ctl.Request) { o.runtime = nil }},
		{name: "portable-bundle", adjust: func(_ *RunOptions, r *ctl.Request) { r.Mode = ctl.SnapshotModeBundle }},
		{name: "older-managed-host", adjust: func(o *RunOptions, _ *ctl.Request) {
			o.CurrentRun = &artifact.CurrentRun{SandboxID: "sid", InstanceID: "instance", RunID: "run"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := RunOptions{
				EnableCheckpoint: true, runtime: &Runtime{}, checkpointDir: t.TempDir(),
				MemoryBinding: &MemorySourceBinding{SnapshotRef: "invalid://memory"},
			}
			req := ctl.Request{CheckpointID: "next"}
			tc.adjust(&opts, &req)
			// The synchronous path retains its own graph validation. The fast
			// preflight must not resolve this deliberately invalid binding.
			save, err := prepareCheckpointMemorySave(context.Background(), req, opts, nil, "")
			if save != nil || err != nil {
				t.Fatalf("non-fast capture entered fast graph preparation: %v %v", save, err)
			}
		})
	}
}

func checkpointMemoryRefLimitOptions(t *testing.T, localCount, externalCount int) RunOptions {
	t.Helper()
	dir := t.TempDir()
	external := make([]string, externalCount)
	for i := range external {
		external[i] = fmt.Sprintf("manifest://%064x", i+1)
	}
	if localCount == 0 {
		if len(external) == 0 {
			return RunOptions{checkpointDir: dir}
		}
		return RunOptions{checkpointDir: dir, MemoryBinding: &MemorySourceBinding{
			SnapshotRef: external[0], RelativeDir: dir, FromRefs: external[1:],
		}}
	}
	sink := snapshot.NewFileSink(dir, "sid", nil, false, nil)
	t.Cleanup(func() {
		if err := sink.Close(); err != nil {
			t.Error(err)
		}
	})
	refs := append([]string(nil), external...)
	for i := 0; i < localCount; i++ {
		cfg, err := snapshot.MarshalConfig(&snapshot.Config{
			Version: snapshot.SnapshotConfigVersion, SandboxRef: "manifest://" + strings.Repeat("e", 64),
		})
		if err != nil {
			t.Fatal(err)
		}
		source := lifecycleSnapshotSource(t, bytes.Repeat([]byte{byte(i + 1)}, 4096), cfg)
		ref, _, err := sink.AbsorbSnapshot(context.Background(), source)
		if err != nil {
			t.Fatal(err)
		}
		refs = append([]string{ref}, refs...)
	}
	return RunOptions{checkpointDir: dir, MemoryBinding: &MemorySourceBinding{
		SnapshotRef: refs[0], RelativeDir: dir, FromRefs: refs[1:],
	}}
}
