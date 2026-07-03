package stdio

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
)

func ptr(b bool) *bool { return &b }

func TestFromFlags_DefaultPipeMode(t *testing.T) {
	// In `go test`, stdin/stdout are not terminals → auto-detect = pipe mode.
	m, err := FromFlags(nil, nil, nil, "", "", "", nil, "")
	if err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if m.TTY {
		t.Fatal("TTY=true unexpectedly (test stdio isn't a terminal)")
	}
	if m.Stdin.Kind != StreamNone {
		t.Errorf("default stdin = %v, want None", m.Stdin.Kind)
	}
	if m.Stdout.Kind != StreamInherit || m.Stderr.Kind != StreamInherit {
		t.Errorf("default stdout/stderr should be Inherit; got %v / %v", m.Stdout.Kind, m.Stderr.Kind)
	}
	if m.Console.Kind != ConsoleStderr {
		t.Errorf("default console = %v, want Stderr", m.Console.Kind)
	}
}

func TestFromFlags_PipeChannels(t *testing.T) {
	m, _ := FromFlags(ptr(true), nil, ptr(false), "", "", "", nil, "")
	if m.Stdin.Kind != StreamInherit {
		t.Errorf("--stdin: %v", m.Stdin.Kind)
	}
	if m.Stderr.Kind != StreamNone {
		t.Errorf("--stderr=false: %v", m.Stderr.Kind)
	}

	m2, _ := FromFlags(nil, nil, nil, "/tmp/in", "/tmp/out", "/tmp/err", nil, "")
	if m2.Stdin != (Stream{Kind: StreamFile, Path: "/tmp/in"}) ||
		m2.Stdout != (Stream{Kind: StreamFile, Path: "/tmp/out"}) ||
		m2.Stderr != (Stream{Kind: StreamFile, Path: "/tmp/err"}) {
		t.Errorf("file redirects: %+v", m2)
	}
}

func TestFromFlags_TTYRequiresTerminal(t *testing.T) {
	if _, err := FromFlags(nil, nil, nil, "", "", "", ptr(true), ""); err == nil {
		t.Fatal("--tty without a terminal should error")
	}
}

func TestFromFlags_Conflicts(t *testing.T) {
	if _, err := FromFlags(ptr(false), nil, nil, "/tmp/x", "", "", nil, ""); err == nil {
		t.Error("--stdin=false + --stdin-from should conflict")
	}
	if _, err := FromFlags(nil, ptr(false), nil, "", "/tmp/x", "", nil, ""); err == nil {
		t.Error("--stdout=false + --stdout-to should conflict")
	}
	if _, err := FromFlags(nil, nil, ptr(false), "", "", "/tmp/x", nil, ""); err == nil {
		t.Error("--stderr=false + --stderr-to should conflict")
	}
}

func TestFromFlags_Console(t *testing.T) {
	for in, want := range map[string]ConsoleKind{"": ConsoleStderr, "default": ConsoleStderr, "off": ConsoleOff} {
		m, err := FromFlags(nil, nil, nil, "", "", "", nil, in)
		if err != nil {
			t.Fatalf("--console=%q: %v", in, err)
		}
		if m.Console.Kind != want {
			t.Errorf("--console=%q → %v, want %v", in, m.Console.Kind, want)
		}
	}
	m, err := FromFlags(nil, nil, nil, "", "", "", nil, "file=/var/log/k.log")
	if err != nil || m.Console.Kind != ConsoleFile || m.Console.Path != "/var/log/k.log" {
		t.Fatalf("--console=file=...: m=%+v err=%v", m, err)
	}
	if _, err := FromFlags(nil, nil, nil, "", "", "", nil, "weird"); err == nil {
		t.Fatal("--console=weird should error")
	}
	if _, err := FromFlags(nil, nil, nil, "", "", "", nil, "file="); err == nil {
		t.Fatal("--console=file= (empty) should error")
	}
}

func TestModeProtoSpecAndStreamSet(t *testing.T) {
	tty := Mode{TTY: true}
	if !tty.ProtoSpec().TTY || !tty.StreamSet()[mux.StreamPTY] || tty.StreamSet()[mux.StreamStdin] {
		t.Errorf("tty mapping wrong: %+v / %v", tty.ProtoSpec(), tty.StreamSet())
	}
	pipe := Mode{Stdin: Stream{Kind: StreamInherit}, Stdout: Stream{Kind: StreamInherit}, Stderr: Stream{Kind: StreamNone}}
	ps := pipe.ProtoSpec()
	if !ps.Stdin || !ps.Stdout || ps.Stderr || ps.TTY {
		t.Errorf("pipe ProtoSpec wrong: %+v", ps)
	}
	ss := pipe.StreamSet()
	if !ss[mux.StreamStdin] || !ss[mux.StreamStdout] || ss[mux.StreamStderr] || ss[mux.StreamPTY] {
		t.Errorf("pipe StreamSet wrong: %v", ss)
	}
}

func TestSetupCHStdio(t *testing.T) {
	// console off
	cmd := &exec.Cmd{}
	arg, cleanup, err := Mode{Console: Console{Kind: ConsoleOff}}.SetupCHStdio(cmd)
	if err != nil || arg != "off" || cmd.Stdout != nil || cmd.Stderr != os.Stderr {
		t.Fatalf("off: arg=%q stdout=%v err=%v", arg, cmd.Stdout, err)
	}
	cleanup()
	if cmd.Stdin != nil {
		t.Errorf("cmd.Stdin should be nil (→ /dev/null), got %v", cmd.Stdin)
	}

	// console default, pipe mode → cmd.Stdout wraps os.Stderr in a
	// non-*os.File so exec creates an internal pipe (Fix A: prevents
	// CH's --console tty from treating its stdout as a real terminal
	// — which would EPERM on TIOCSCTTY and silently grab the host
	// terminal into raw mode).
	cmd = &exec.Cmd{}
	arg, cleanup, err = Mode{Console: Console{Kind: ConsoleStderr}}.SetupCHStdio(cmd)
	if err != nil || arg != "tty" {
		t.Fatalf("default/pipe: arg=%q err=%v", arg, err)
	}
	if cmd.Stdout == nil {
		t.Fatalf("default/pipe: cmd.Stdout is nil")
	}
	if _, ok := cmd.Stdout.(*os.File); ok {
		t.Fatalf("default/pipe: cmd.Stdout is *os.File — exec would inherit the fd; Fix A regressed")
	}
	cleanup()

	// console file
	dir := t.TempDir()
	p := filepath.Join(dir, "k.log")
	cmd = &exec.Cmd{}
	arg, cleanup, err = Mode{Console: Console{Kind: ConsoleFile, Path: p}}.SetupCHStdio(cmd)
	if err != nil || arg != "tty" {
		t.Fatalf("file: arg=%q err=%v", arg, err)
	}
	if _, ok := cmd.Stdout.(*os.File); !ok {
		t.Fatalf("file: cmd.Stdout should be *os.File, got %T", cmd.Stdout)
	}
	cleanup()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("console file not created: %v", err)
	}
}

func TestCRLF(t *testing.T) {
	var buf bytes.Buffer
	w := CRLF(&buf)
	n, err := w.Write([]byte("a\nb\nc"))
	if err != nil || n != 5 {
		t.Fatalf("Write: n=%d err=%v", n, err)
	}
	if buf.String() != "a\r\nb\r\nc" {
		t.Fatalf("got %q", buf.String())
	}
	buf.Reset()
	if _, err := w.Write([]byte("no newline")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "no newline" {
		t.Fatalf("got %q", buf.String())
	}
}

// TestBridgePipeStdout: a guest-side mux session writes to its STDOUT
// stream; the host-side Bridge (pipe mode, --stdout-to FILE) should land
// the bytes in the file.
func TestBridgePipeStdout(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "app.out")
	mode := Mode{Stdin: Stream{Kind: StreamNone}, Stdout: Stream{Kind: StreamFile, Path: out}, Stderr: Stream{Kind: StreamNone}, Console: Console{Kind: ConsoleStderr}}

	hc, gc := net.Pipe()
	ss := mux.PipeStreams(false, true, false) // only stdout
	hostSess := mux.NewSession(hc, ss, mux.Options{})
	guestSess := mux.NewSession(gc, ss, mux.Options{})
	defer hostSess.Close()
	defer guestSess.Close()

	cleanup, err := mode.Bridge(context.Background(), hostSess, ss)
	if err != nil {
		t.Fatalf("Bridge: %v", err)
	}

	const payload = "hello from the guest app\n"
	if _, err := guestSess.Stream(mux.StreamStdout).Write([]byte(payload)); err != nil {
		t.Fatalf("guest stdout write: %v", err)
	}
	if err := guestSess.Stream(mux.StreamStdout).CloseWrite(); err != nil {
		t.Fatalf("guest stdout CloseWrite: %v", err)
	}
	// cleanup waits for the host→file pump to drain (it ends on EOF).
	done := make(chan struct{})
	go func() { cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge cleanup did not return (pump not drained)")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("got %q want %q", got, payload)
	}
}
