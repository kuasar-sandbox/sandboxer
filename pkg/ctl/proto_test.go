package ctl

import (
	"bytes"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

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
