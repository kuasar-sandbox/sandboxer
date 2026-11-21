package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func startCheckpointDeleteServer(t *testing.T, handler func(Request) error) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctl.sock")
	server := &Server{Path: path, CheckpointDeleteHandler: handler}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		server.Stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return path
}

func TestDeleteCheckpointsStreamsCompleteRetentionGraph(t *testing.T) {
	sources := CheckpointSources{Retain: make([]CheckpointSource, 400), RefLocations: map[string]string{"parent": "/parent"}}
	for i := range sources.Retain {
		sources.Retain[i] = CheckpointSource{SnapshotRef: fmt.Sprintf("manifest://%064x", i+1), SandboxRef: fmt.Sprintf("manifest://%064x", i+401)}
	}
	raw, err := json.Marshal(sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= MaxMessageBytes {
		t.Fatal("fixture must exceed a management frame")
	}
	var wantSources CheckpointSources
	if err := json.Unmarshal(raw, &wantSources); err != nil {
		t.Fatal(err)
	}
	refs := make([]CheckpointRef, 129)
	for i := range refs {
		refs[i] = CheckpointRef{DiffID: "disk", ID: fmt.Sprintf("v%d", i)}
	}
	wantRefs := append([]CheckpointRef(nil), refs...)
	var calls atomic.Int32
	socket := startCheckpointDeleteServer(t, func(req Request) error {
		batch := int(calls.Add(1)) - 1
		if batch >= 2 || req.Type != TypeCheckpointDeleteRequest || req.CheckpointOwner != "owner" || !reflect.DeepEqual(req.CheckpointSources, &wantSources) || !reflect.DeepEqual(req.CheckpointRefs, wantRefs[batch*128:min((batch+1)*128, len(wantRefs))]) {
			return fmt.Errorf("batch %d did not preserve complete exact input", batch)
		}
		// The first accepted batch must not let caller memory retarget a retry.
		refs[128].ID = "replacement"
		sources.Retain[399].SnapshotRef = "replacement"
		sources.RefLocations["parent"] = "/replacement"
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := DeleteCheckpoints(ctx, socket, "owner", refs, sources); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
}

func TestCheckpointDeleteStreamRequiresCompleteCommit(t *testing.T) {
	req := Request{Type: TypeCheckpointDeleteRequest, CheckpointOwner: "owner", CheckpointRefs: []CheckpointRef{{ID: "one", MemoryID: "memory"}}, CheckpointSources: &CheckpointSources{Retain: []CheckpointSource{}}}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	data := checkpointDeleteFrame{Type: typeCheckpointDeleteChunk, Data: body}
	commit := checkpointDeleteFrame{Type: typeCheckpointDeleteCommit}
	var complete bytes.Buffer
	if err := WriteMessage(&complete, data); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(&complete, commit); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), complete.Bytes()...)
	got, err := readCheckpointDeleteRequest(&complete, len(body))
	if err != nil || !reflect.DeepEqual(got, req) {
		t.Fatalf("complete: %+v %v", got, err)
	}
	for _, cut := range []int{0, 4, len(wire) / 2, len(wire) - 1} {
		if got, err := readCheckpointDeleteRequest(bytes.NewReader(wire[:cut]), len(body)); err == nil || got.Type != "" {
			t.Fatalf("partial request at %d: %+v %v", cut, got, err)
		}
	}
	for name, frames := range map[string][]checkpointDeleteFrame{
		"no-commit":       {data},
		"empty-data":      {{Type: typeCheckpointDeleteChunk}, data, commit},
		"wrong-type":      {{Type: TypeCheckpointDeleteDone}},
		"early-commit":    {commit},
		"extra-data":      {data, data, commit},
		"nonempty-commit": {data, {Type: typeCheckpointDeleteCommit, Data: []byte("x")}},
		"invalid-json":    {{Type: typeCheckpointDeleteChunk, Data: bytes.Repeat([]byte("x"), len(body))}, commit},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			for _, f := range frames {
				if err := WriteMessage(&buf, f); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := readCheckpointDeleteRequest(&buf, len(body)); err == nil || got.Type != "" {
				t.Fatalf("invalid stream returned deletion: %+v %v", got, err)
			}
		})
	}
}

func TestCheckpointDeleteStreamDisconnectDoesNotInvokeHandler(t *testing.T) {
	var calls atomic.Int32
	socket := startCheckpointDeleteServer(t, func(Request) error { calls.Add(1); return nil })
	addr, err := net.ResolveUnixAddr("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUnix("unix", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(conn, Request{Type: TypeCheckpointDeleteStreamRequest, BodyBytes: MaxMessageBytes + 1}); err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := ReadMessage(conn, &response); err != nil || response.Type != TypeCheckpointDeleteReady {
		t.Fatalf("readiness %+v: %v", response, err)
	}
	if err := WriteMessage(conn, checkpointDeleteFrame{Type: typeCheckpointDeleteChunk, Data: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if err := ReadMessage(conn, &response); err != nil || response.Type != TypeError {
		t.Fatalf("incomplete response %+v: %v", response, err)
	}
	if calls.Load() != 0 {
		t.Fatal("incomplete retention input reached deletion")
	}
}
