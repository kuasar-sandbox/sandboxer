package proto

import (
	"bytes"
	"reflect"
	"testing"
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

func TestRoundTripWithShortWrites(t *testing.T) {
	want := &Message{
		Type:  TypeError,
		Msg:   string(bytes.Repeat([]byte("short-write-"), 1024)),
		Epoch: 17,
	}
	w := &shortWriter{max: 3}
	if err := WriteMessage(w, want); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	got, err := ReadMessage(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, want)
	}
}

func TestRoundTrip_Launch(t *testing.T) {
	m := &Message{
		Type: TypeLaunch,
		Launch: &LaunchSpec{
			Exec:    "/usr/bin/foo",
			Args:    []string{"--flag", "value with spaces", "comma,inside,arg"},
			Env:     map[string]string{"PATH": "/bin:/usr/bin", "HOME": "/root"},
			Workdir: "/var/data",
			Restart: "on-failure",
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("roundtrip mismatch:\n got=%+v\nwant=%+v", got, m)
	}
}

func TestRoundTrip_LaunchPodSpec(t *testing.T) {
	m := &Message{
		Type: TypeLaunch,
		Launch: &LaunchSpec{
			Exec:         "/usr/bin/foo",
			User:         "app:app",
			StopSignal:   15,
			StopGraceSec: 30,
			Mounts: []MountSpec{
				{Target: "/tmp", Type: "tmpfs", Options: "nosuid,nodev,mode=1777"},
				{Target: "/var/log", Type: "empty"},
			},
			Files: []FileSpec{
				{Path: "/etc/resolv.conf", Content: "nameserver 1.2.3.4\n", Mode: "0644", Owner: "0:0", ReadOnly: true},
			},
			Init: []InitSpec{
				{Exec: "/bin/sh", Args: []string{"-c", "echo hi"}, User: "0:0"},
			},
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("roundtrip mismatch:\n got=%+v\nwant=%+v", got, m)
	}
}

func TestRoundTrip_RestoreFiles(t *testing.T) {
	m := &Message{
		Type:        TypeRestore,
		Epoch:       2,
		WallclockNs: 123456789,
		Files: []FileSpec{
			{Path: "/run/secrets/token", Content: "s3cr3t", Mode: "0400"},
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Errorf("roundtrip mismatch:\n got=%+v\nwant=%+v", got, m)
	}
}

func TestRoundTrip_Hello(t *testing.T) {
	m := &Message{Type: TypeHello, Phase: "ready"}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadMessage(&buf)
	if got.Type != TypeHello || got.Phase != "ready" {
		t.Errorf("got %+v", got)
	}
}

func TestRoundTrip_LaunchWithNetwork(t *testing.T) {
	m := &Message{
		Type: TypeLaunch,
		Launch: &LaunchSpec{
			Exec: "/usr/bin/foo",
			Network: &NetworkSpec{
				Interface: "eth0",
				IPCIDR:    "169.254.1.1/31",
				Nexthop:   "169.254.1.0",
				Hostname:  "test-sandbox",
			},
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Launch.Network, m.Launch.Network) {
		t.Errorf("Network mismatch:\n got=%+v\nwant=%+v", got.Launch.Network, m.Launch.Network)
	}
}

func TestRoundTrip_LaunchWithoutNetwork(t *testing.T) {
	// nil Network omitempty — backward compatible with old sandbox-init.
	m := &Message{
		Type:   TypeLaunch,
		Launch: &LaunchSpec{Exec: "/usr/bin/foo"},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadMessage(&buf)
	if got.Launch.Network != nil {
		t.Errorf("Network should be nil when omitted, got %+v", got.Launch.Network)
	}
}

func TestRead_TooLarge(t *testing.T) {
	var buf bytes.Buffer
	// header says 1 MiB, exceeds MaxMessageBytes
	buf.Write([]byte{0, 0, 0x10, 0})
	_, err := ReadMessage(&buf)
	if err == nil {
		t.Fatal("expected error for oversized message")
	}
}

func TestRead_TruncatedHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{0x10, 0x00}) // only 2 bytes
	_, err := ReadMessage(&buf)
	if err == nil {
		t.Fatal("expected error on truncated header")
	}
}

func TestWrite_TooLarge(t *testing.T) {
	big := make([]byte, MaxMessageBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	m := &Message{Type: TypeHello, Phase: string(big)}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestRoundTrip_PingPong(t *testing.T) {
	ping := &Message{Type: TypePing, ID: 42, TSendNs: 1715000000000000000}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, ping); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != TypePing || got.ID != 42 || got.TSendNs != 1715000000000000000 {
		t.Errorf("ping round-trip: got %+v", got)
	}

	pong := &Message{Type: TypePong, ID: 42, TSendNs: 1715000000000000000}
	buf.Reset()
	if err := WriteMessage(&buf, pong); err != nil {
		t.Fatal(err)
	}
	got, _ = ReadMessage(&buf)
	if got.Type != TypePong || got.ID != 42 {
		t.Errorf("pong round-trip: got %+v", got)
	}
}

func TestRoundTrip_RestoreEpoch(t *testing.T) {
	m := &Message{Type: TypeRestore, Epoch: 7}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadMessage(&buf)
	if got.Type != TypeRestore || got.Epoch != 7 {
		t.Errorf("restore round-trip: got %+v", got)
	}
}

func TestRoundTrip_AppLifecycle(t *testing.T) {
	for _, tc := range []*Message{
		{Type: TypeAppStarted, PID: 4711},
		{Type: TypeAppExited, Code: 137},
		{Type: TypeQuiesce},
		{Type: TypeQuiesced},
		{Type: TypeAck},
		{Type: TypeError, Msg: "bad request"},
	} {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, tc); err != nil {
			t.Fatalf("write %s: %v", tc.Type, err)
		}
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("read %s: %v", tc.Type, err)
		}
		if got.Type != tc.Type {
			t.Errorf("%s: type mismatch %q", tc.Type, got.Type)
		}
		if tc.PID != 0 && got.PID != tc.PID {
			t.Errorf("%s: pid mismatch %d", tc.Type, got.PID)
		}
		if tc.Code != 0 && got.Code != tc.Code {
			t.Errorf("%s: code mismatch %d", tc.Type, got.Code)
		}
	}
}

func TestRoundTrip_Connect(t *testing.T) {
	cases := []*Message{
		{Type: TypeConnect, Connect: &ConnectSpec{Address: "127.0.0.1:49983"}},
		{Type: TypeConnect, Connect: &ConnectSpec{Network: "tcp6", Address: "[::1]:8080"}},
		{Type: TypeConnect, Connect: &ConnectSpec{Network: "unix", Address: "/run/up.sock"}},
		{Type: TypeConnect, Connect: &ConnectSpec{Network: "tcp", Address: "0.0.0.0:8080", Accept: true}},
		{Type: TypeConnect, Connect: &ConnectSpec{Network: "unix", Address: "/run/up.sock", Accept: true}},
		{Type: TypeConnectAck},
	}
	for _, m := range cases {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatalf("write %s: %v", m.Type, err)
		}
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("read %s: %v", m.Type, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Errorf("%s round-trip mismatch:\n got=%+v\nwant=%+v", m.Type, got, m)
		}
	}
}

func TestHostConnectLine(t *testing.T) {
	want := "CONNECT 5000\n"
	if string(HostConnectLine) != want {
		t.Errorf("HostConnectLine = %q, want %q", HostConnectLine, want)
	}
}

func TestRoundTrip_LaunchWithStdio(t *testing.T) {
	m := &Message{
		Type: TypeLaunch,
		Launch: &LaunchSpec{
			Exec:  "/bin/sh",
			Stdio: StdioSpec{TTY: true, Winsize: &Winsize{Cols: 132, Rows: 50}},
		},
	}
	var buf bytes.Buffer
	if err := WriteMessage(&buf, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Launch.Stdio, m.Launch.Stdio) {
		t.Errorf("Stdio mismatch:\n got=%+v\nwant=%+v", got.Launch.Stdio, m.Launch.Stdio)
	}

	// pipe mode: per-channel flags
	m2 := &Message{Type: TypeLaunch, Launch: &LaunchSpec{Exec: "/bin/cat", Stdio: StdioSpec{Stdin: true, Stdout: true}}}
	buf.Reset()
	if err := WriteMessage(&buf, m2); err != nil {
		t.Fatal(err)
	}
	got2, _ := ReadMessage(&buf)
	if !got2.Launch.Stdio.Stdin || !got2.Launch.Stdio.Stdout || got2.Launch.Stdio.Stderr || got2.Launch.Stdio.TTY {
		t.Errorf("pipe-mode Stdio mismatch: %+v", got2.Launch.Stdio)
	}
}

func TestRoundTrip_AttachAndAck(t *testing.T) {
	cases := []*Message{
		{Type: TypeAttach, Epoch: 3},
		{Type: TypeAttachAck, Stdio: &StdioSpec{TTY: true}, AppState: AppStateRunning},
		{Type: TypeRestoreAck, Stdio: &StdioSpec{Stdin: true, Stdout: true, Stderr: true}, AppState: AppStateExited, Code: 137, TermSignal: 9},
		{Type: TypeLaunchAck, Stdio: &StdioSpec{Stdout: true}},
		{Type: TypeAppExited, Code: 1, TermSignal: 0},
	}
	for _, m := range cases {
		var buf bytes.Buffer
		if err := WriteMessage(&buf, m); err != nil {
			t.Fatalf("write %s: %v", m.Type, err)
		}
		got, err := ReadMessage(&buf)
		if err != nil {
			t.Fatalf("read %s: %v", m.Type, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Errorf("%s round-trip mismatch:\n got=%+v\nwant=%+v", m.Type, got, m)
		}
	}
}
