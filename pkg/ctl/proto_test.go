package ctl

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

type shortWriter struct {
	bytes.Buffer
	max int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

func TestMessageRoundTripWithShortWrites(t *testing.T) {
	want := Response{
		Type: TypeSnapshotDone,
		Msg:  string(bytes.Repeat([]byte("short-write-"), 1024)),
	}
	w := &shortWriter{max: 3}
	if err := WriteMessage(w, &want); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var got Response
	if err := ReadMessage(bytes.NewReader(w.Bytes()), &got); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, want)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	req := Request{
		Type: TypeExecRequest,
		Exec: &proto.ExecSpec{
			Argv:  []string{"/bin/sh", "-c", "echo hi"},
			Env:   map[string]string{"FOO": "bar"},
			Cwd:   "/work",
			Stdio: proto.StdioSpec{TTY: true, Winsize: &proto.Winsize{Cols: 80, Rows: 24}},
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &req); err != nil {
		t.Fatalf("WriteMessage req: %v", err)
	}
	var got Request
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatalf("ReadMessage req: %v", err)
	}
	if got.Type != TypeExecRequest || got.Exec == nil ||
		len(got.Exec.Argv) != 3 || got.Exec.Argv[2] != "echo hi" ||
		got.Exec.Env["FOO"] != "bar" || got.Exec.Cwd != "/work" ||
		!got.Exec.Stdio.TTY || got.Exec.Stdio.Winsize == nil ||
		got.Exec.Stdio.Winsize.Cols != 80 {
		t.Fatalf("req round-trip mismatch: %+v", got.Exec)
	}

	resp := Response{Type: TypeExecAck, Stdio: &proto.StdioSpec{Stdout: true, Stderr: true}}
	buf.Reset()
	if err := WriteMessage(&buf, &resp); err != nil {
		t.Fatalf("WriteMessage resp: %v", err)
	}
	var gotResp Response
	if err := ReadMessage(&buf, &gotResp); err != nil {
		t.Fatalf("ReadMessage resp: %v", err)
	}
	if gotResp.Type != TypeExecAck || gotResp.Stdio == nil ||
		!gotResp.Stdio.Stdout || !gotResp.Stdio.Stderr || gotResp.Stdio.Stdin {
		t.Fatalf("resp round-trip mismatch: %+v", gotResp)
	}
}

func TestReadMessageOversized(t *testing.T) {
	var buf bytes.Buffer
	// length prefix claiming > MaxMessageBytes must be rejected.
	buf.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	var got Request
	if err := ReadMessage(&buf, &got); err == nil {
		t.Fatal("expected oversized-message error, got nil")
	}
}

func TestSnapshotBoolDefaultsAndRoundTrip(t *testing.T) {
	if (Request{}).DropCachesEnabled() || !(Request{}).MergeRefEnabled() {
		t.Fatal("drop_caches must default false and merge_ref must default true")
	}

	f := false
	want := Request{Type: TypeSnapshotRequest, DropCaches: &f, MergeRef: &f}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &want); err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.DropCaches == nil || *got.DropCaches || got.DropCachesEnabled() {
		t.Fatalf("drop_caches false was not preserved: %+v", got.DropCaches)
	}
	if got.MergeRef == nil || *got.MergeRef || got.MergeRefEnabled() {
		t.Fatalf("merge_ref false was not preserved: %+v", got.MergeRef)
	}

	trueValue := true
	if !(Request{DropCaches: &trueValue}).DropCachesEnabled() {
		t.Fatal("drop_caches true was not enabled")
	}
}

func TestSnapshotModeDefaultValidationAndRoundTrip(t *testing.T) {
	if got, err := (Request{}).SnapshotMode(); err != nil || got != SnapshotModeLocal {
		t.Fatalf("default mode = %q, err=%v", got, err)
	}
	want := Request{Type: TypeSnapshotRequest, Mode: SnapshotModeBundle}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &want); err != nil {
		t.Fatal(err)
	}
	var got Request
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if mode, err := got.SnapshotMode(); err != nil || mode != SnapshotModeBundle {
		t.Fatalf("round-trip mode = %q, err=%v", mode, err)
	}
	if _, err := (Request{Mode: "remote"}).SnapshotMode(); err == nil {
		t.Fatal("legacy remote mode was accepted")
	}
}

func TestSnapshotRequestOmitsUnsetBoolFields(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &Request{Type: TypeSnapshotRequest}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("drop_caches")) || bytes.Contains(buf.Bytes(), []byte("merge_ref")) {
		t.Fatalf("unset bool fields must be omitted: %q", buf.Bytes())
	}
}

func TestSnapshotProtocolKeepsStagingAndTypedDropCachesResult(t *testing.T) {
	wantReq := Request{Type: TypeSnapshotRequest, StagingDir: "/run/sandbox/snap-stage"}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, &wantReq); err != nil {
		t.Fatal(err)
	}
	var gotReq Request
	if err := ReadMessage(&buf, &gotReq); err != nil {
		t.Fatal(err)
	}
	if gotReq.StagingDir != wantReq.StagingDir {
		t.Fatalf("staging_dir = %q, want %q", gotReq.StagingDir, wantReq.StagingDir)
	}

	wantResp := Response{Type: TypeSnapshotDone, DropCachesResult: proto.DropCachesSkipped}
	buf.Reset()
	if err := WriteMessage(&buf, &wantResp); err != nil {
		t.Fatal(err)
	}
	var gotResp Response
	if err := ReadMessage(&buf, &gotResp); err != nil {
		t.Fatal(err)
	}
	if gotResp.DropCachesResult != proto.DropCachesSkipped {
		t.Fatalf("drop_caches_result = %q, want %q", gotResp.DropCachesResult, proto.DropCachesSkipped)
	}
}
