package ctl

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestReadExecRequestFrameValidPreservesRawAndTail(t *testing.T) {
	payload := []byte(" \n{\"exec\":{\"argv\":[\"/bin/sh\",\"-c\",\"true\"],\"env\":{\"B\":\"2\",\"A\":\"1\"},\"cwd\":\"/work\",\"user\":\"1000:1000\",\"stdio\":{\"tty\":true,\"winsize\":{\"cols\":91,\"rows\":37},\"stdin\":true,\"stdout\":true,\"stderr\":true}},\"type\":\"exec_request\"}\t")
	raw := ctlTestFrame(payload)
	tail := []byte{0, 1, 2, 3, 4, 5}
	reader := bytes.NewReader(append(append([]byte(nil), raw...), tail...))

	frame, err := ReadExecRequestFrame(reader)
	if err != nil {
		t.Fatalf("ReadExecRequestFrame: %v", err)
	}
	if !bytes.Equal(frame.Raw, raw) {
		t.Fatalf("Raw changed:\n got %q\nwant %q", frame.Raw, raw)
	}
	want := Request{
		Type: TypeExecRequest,
		Exec: &proto.ExecSpec{
			Argv: []string{"/bin/sh", "-c", "true"},
			Env:  map[string]string{"A": "1", "B": "2"},
			Cwd:  "/work",
			User: "1000:1000",
			Stdio: proto.StdioSpec{
				TTY: true, Winsize: &proto.Winsize{Cols: 91, Rows: 37},
				Stdin: true, Stdout: true, Stderr: true,
			},
		},
	}
	if !reflect.DeepEqual(frame.Request, want) {
		t.Fatalf("Request = %#v, want %#v", frame.Request, want)
	}
	gotTail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !bytes.Equal(gotTail, tail) {
		t.Fatalf("tail = %v, want %v", gotTail, tail)
	}
}

func TestReadExecRequestFramePreservesCurrentOptionalNullSemantics(t *testing.T) {
	frame, err := ReadExecRequestFrame(bytes.NewReader(ctlTestFrame([]byte(
		`{"type":"exec_request","exec":{"argv":["true"],"env":null,"cwd":null,"user":null,"stdio":null}}`,
	))))
	if err != nil {
		t.Fatalf("ReadExecRequestFrame: %v", err)
	}
	if frame.Request.Exec == nil || frame.Request.Exec.Env != nil ||
		frame.Request.Exec.Cwd != "" || frame.Request.Exec.User != "" ||
		frame.Request.Exec.Stdio != (proto.StdioSpec{}) {
		t.Fatalf("optional null wire semantics changed: %#v", frame.Request.Exec)
	}
}

func TestReadExecRequestFrameRejectsInvalidInput(t *testing.T) {
	lengthOnly := func(n uint32, payload string) []byte {
		var prefix [4]byte
		binary.LittleEndian.PutUint32(prefix[:], n)
		return append(prefix[:], payload...)
	}
	var oversized [4]byte
	binary.LittleEndian.PutUint32(oversized[:], MaxMessageBytes+1)

	tests := []struct {
		name  string
		input []byte
	}{
		{name: "oversized", input: oversized[:]},
		{name: "truncated length", input: []byte{1, 2, 3}},
		{name: "truncated payload", input: lengthOnly(8, `{}`)},
		{name: "malformed JSON", input: ctlTestFrame([]byte(`{"type":`))},
		{name: "second JSON value", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"]}} {}`))},
		{name: "top-level null", input: ctlTestFrame([]byte(`null`))},
		{name: "top-level array", input: ctlTestFrame([]byte(`[]`))},
		{name: "duplicate top field", input: ctlTestFrame([]byte(`{"type":"exec_request","type":"exec_request","exec":{"argv":["true"]}}`))},
		{name: "unknown top field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"]},"unknown":false}`))},
		{name: "snapshot request", input: ctlTestFrame([]byte(`{"type":"snapshot_request","exec":{"argv":["true"]}}`))},
		{name: "exec null", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":null}`))},
		{name: "exec missing", input: ctlTestFrame([]byte(`{"type":"exec_request"}`))},
		{name: "argv missing", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{}}`))},
		{name: "argv null", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":null}}`))},
		{name: "argv empty", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":[]}}`))},
		{name: "snapshot field mixed in", input: ctlTestFrame([]byte(`{"type":"exec_request","out_dir":"/tmp","exec":{"argv":["true"]}}`))},
		{name: "duplicate exec field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"cwd":"/a","cwd":"/b"}}`))},
		{name: "unknown exec field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"args":[]}}`))},
		{name: "duplicate env key", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"env":{"A":"1","A":"2"}}}`))},
		{name: "duplicate stdio field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"stdio":{"tty":true,"tty":false}}}`))},
		{name: "unknown stdio field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"stdio":{"pty":true}}}`))},
		{name: "duplicate winsize field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"stdio":{"winsize":{"cols":80,"cols":81,"rows":24}}}}`))},
		{name: "unknown winsize field", input: ctlTestFrame([]byte(`{"type":"exec_request","exec":{"argv":["true"],"stdio":{"winsize":{"cols":80,"rows":24,"pixels":1}}}}`))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if frame, err := ReadExecRequestFrame(bytes.NewReader(test.input)); err == nil {
				t.Fatalf("accepted invalid frame: %#v", frame)
			}
		})
	}
}

func TestReadExecRequestFrameTTYKeepsIgnoredPipeFlags(t *testing.T) {
	frame, err := ReadExecRequestFrame(bytes.NewReader(ctlTestFrame([]byte(
		`{"type":"exec_request","exec":{"argv":["true"],"stdio":{"tty":true,"stdin":true,"stdout":true,"stderr":true}}}`,
	))))
	if err != nil {
		t.Fatalf("ReadExecRequestFrame rejected existing TTY wire semantics: %v", err)
	}
	stdio := frame.Request.Exec.Stdio
	if !stdio.TTY || !stdio.Stdin || !stdio.Stdout || !stdio.Stderr {
		t.Fatalf("stdio flags changed during parsing: %+v", stdio)
	}
}
