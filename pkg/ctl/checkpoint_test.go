package ctl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDeleteDiffCheckpointsKeepsExactBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	refs := make([]DiffCheckpointRef, 129)
	for i := range refs {
		refs[i] = DiffCheckpointRef{DiffID: "disk", ID: fmt.Sprintf("v%d", i)}
	}
	want := append([]DiffCheckpointRef(nil), refs...)
	done := make(chan error, 1)
	go func() {
		for batch := 0; batch < 2; batch++ {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			var req Request
			err = ReadMessage(conn, &req)
			if err == nil && (req.Type != TypeCheckpointDeleteRequest || req.CheckpointOwner != "owner" ||
				!reflect.DeepEqual(req.CheckpointRefs, want[batch*128:min((batch+1)*128, len(want))])) {
				err = fmt.Errorf("unexpected batch %d: %+v", batch, req)
			}
			if err == nil {
				// The first request proves that the client has frozen the set.
				// Changing the caller's final item must not alter the next batch.
				refs[len(refs)-1].ID = "replacement"
				err = WriteMessage(conn, Response{Type: TypeCheckpointDeleteDone})
			}
			_ = conn.Close()
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := DeleteDiffCheckpoints(ctx, path, "owner", refs); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeleteDiffCheckpointsEmptySetDoesNotConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := DeleteDiffCheckpoints(ctx, "missing.sock", "owner", nil); err != nil {
		t.Fatal("empty deletion stopped being a no-op", err)
	}
}

func TestCheckpointListRequiresCompleteTerminator(t *testing.T) {
	refs := make([]DiffCheckpointRef, 2000)
	for i := range refs {
		refs[i] = DiffCheckpointRef{DiffID: fmt.Sprintf("disk-%04d", i), ID: "same-capture"}
	}
	var wire bytes.Buffer
	if err := WriteCheckpointList(&wire, refs); err != nil {
		t.Fatal(err)
	}
	encoded := append([]byte(nil), wire.Bytes()...)
	got, err := ReadCheckpointList(&wire)
	if err != nil || len(got) != len(refs) {
		t.Fatalf("complete list %d: %v", len(got), err)
	}
	for _, cut := range []int{0, 4, 60, len(encoded) / 2, len(encoded) - 1} {
		got, err := ReadCheckpointList(bytes.NewReader(encoded[:cut]))
		if err == nil || got != nil {
			t.Fatalf("truncated at %d returned success/partial list", cut)
		}
	}
	var missing bytes.Buffer
	if err := WriteMessage(&missing, Response{Type: TypeCheckpointListChunk, Checkpoints: refs[:1]}); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadCheckpointList(&missing); err == nil || got != nil {
		t.Fatal("missing done frame accepted")
	}
}

func TestCheckpointListRejectsWrongCountAndErrorAfterData(t *testing.T) {
	for _, last := range []Response{{Type: TypeCheckpointListDone, CheckpointCount: 2}, {Type: TypeError, Msg: "disk read failed"}, {Type: TypeSnapshotDone}} {
		var wire bytes.Buffer
		_ = WriteMessage(&wire, Response{Type: TypeCheckpointListChunk, Checkpoints: []DiffCheckpointRef{{DiffID: "disk", ID: "c1"}}})
		_ = WriteMessage(&wire, last)
		if got, err := ReadCheckpointList(&wire); err == nil || got != nil {
			t.Fatalf("invalid terminal %q accepted", last.Type)
		}
	}
}

func TestCheckpointUnsupportedDoesNotHideStorageFailures(t *testing.T) {
	for _, r := range []Response{
		{Type: TypeError, Code: CheckpointUnsupportedCode, Msg: "disabled"},
		{Type: TypeError, Msg: "unknown type: " + TypeCheckpointExportRequest},
		{Type: TypeError, Msg: "unknown type: " + TypeCheckpointSnapshotRequest},
	} {
		if !errors.Is(CaptureError(r), ErrCheckpointUnsupported) {
			t.Fatalf("unsupported not recognized: %+v", r)
		}
	}
	for _, msg := range []string{"open: input/output error", "no space left on device", "unknown type: export_request", "checkpoint_unsupported in file name"} {
		if errors.Is(CaptureError(Response{Type: TypeError, Msg: msg}), ErrCheckpointUnsupported) {
			t.Fatalf("storage failure hidden: %s", msg)
		}
	}
}

func TestCheckpointOldOwnerCannotRemoveNewRoute(t *testing.T) {
	dir := t.TempDir()
	old, err := PublishCheckpointOwner(dir, filepath.Join(dir, "old.sock"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := PublishCheckpointOwner(dir, filepath.Join(dir, "new.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if current.ID == old.ID {
		t.Fatal("reused ownership token")
	}
	if err := RemoveCheckpointOwner(dir, old); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCheckpointOwner(dir)
	if err != nil || got != current {
		t.Fatalf("old cleanup lost new route: %+v %v", got, err)
	}
	if err := RemoveCheckpointOwner(dir, current); err != nil {
		t.Fatal(err)
	}
}
