package uffd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestVAReportDiagnosticCallbackBeforeACK(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	connections := make([]*net.UnixConn, 0, 2)
	for _, fd := range pair {
		file := os.NewFile(uintptr(fd), "va-diagnostic-socketpair")
		conn, err := net.FileConn(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		unixConn := conn.(*net.UnixConn)
		connections = append(connections, unixConn)
		t.Cleanup(func() { unixConn.Close() })
	}
	serverConn, client := connections[0], connections[1]
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	adoptedFD := -1
	srv := &VAReportServer{AddrMap: NewAddressMap(PageSize), Logf: func(string, ...any) {}}
	srv.OnReady = func(fd int, va, size, offset uint64) error {
		adoptedFD = fd
		return nil
	}
	srv.OnAdopted = func(d AdoptionDiagnostic) {
		if d.UffdFD != adoptedFD || d.PeerErr != nil || d.PeerPID != os.Getpid() || d.VAStart != unitCHVA || d.Size != PageSize || d.MemfdOffset != 0 {
			t.Errorf("adoption diagnostic=%+v", d)
		}
		if !srv.mu.TryLock() {
			t.Error("diagnostic callback held the server lock")
		} else {
			srv.mu.Unlock()
		}
		close(entered)
		<-release
	}
	go func() { srv.handle(serverConn); close(done) }()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		_ = serverConn.Close()
		receiveSignal(t, done)
		if adoptedFD >= 0 {
			_ = unix.Close(adoptedFD)
		}
	}()
	pipe := loadPipe(t)
	defer unix.Close(pipe[0])
	body, err := json.Marshal(vaReportMsg{Type: "va_report", VAStart: unitCHVA, Size: PageSize})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 4+len(body))
	binary.LittleEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if _, _, err := client.WriteMsgUnix(frame, unix.UnixRights(pipe[0]), nil); err != nil {
		t.Fatal(err)
	}
	receiveSignal(t, entered)
	// The accepted fd is owned, but ACK cannot be published while its
	// diagnostic callback is still running. A nonblocking peek avoids sleeps.
	raw, err := client.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var peekErr error
	if err := raw.Control(func(fd uintptr) {
		var b [4]byte
		_, _, peekErr = unix.Recvfrom(int(fd), b[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
	}); err != nil || !errors.Is(peekErr, unix.EAGAIN) {
		t.Fatalf("ACK escaped before callback return: control=%v receive=%v", err, peekErr)
	}
	close(release)
	var prefix [4]byte
	if _, err := io.ReadFull(client, prefix[:]); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, binary.LittleEndian.Uint32(prefix[:]))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	var ack ackMsg
	if err := json.Unmarshal(response, &ack); err != nil || ack.Type != "ack" {
		t.Fatalf("ACK=%+v err=%v", ack, err)
	}
}

func sendTestVAReport(t *testing.T, path string, fd int, va, size uint64) ackMsg {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	body, err := json.Marshal(vaReportMsg{Type: "va_report", VAStart: va, Size: size})
	if err != nil {
		t.Fatal(err)
	}
	var prefix [4]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(len(body)))
	if _, _, err := conn.WriteMsgUnix(prefix[:], unix.UnixRights(fd), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(body); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, binary.LittleEndian.Uint32(prefix[:]))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	var ack ackMsg
	if err := json.Unmarshal(response, &ack); err != nil {
		t.Fatal(err)
	}
	return ack
}

func TestVAReportRollbackAndOffsetHandoff(t *testing.T) {
	m := NewAddressMap(3 * PageSize)
	var handler *Handler
	failFirst := true
	var offsets []uint64
	memfd, err := unix.MemfdCreate("va-unit", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(memfd)
	srv := &VAReportServer{Path: filepath.Join(t.TempDir(), "uffd.sock"), AddrMap: m}
	srv.OnReady = func(fd int, va, size, off uint64) error {
		if failFirst {
			failFirst = false
			return errors.New("injected adoption failure")
		}
		offsets = append(offsets, off)
		var err error
		handler, err = NewWithBackendUffd(fd, m, Config{MemfdFD: memfd, Size: 3 * PageSize, Source: ZeroSource{}, NumWorkers: 2})
		if err == nil {
			handler.Start()
		}
		return err
	}
	srv.OnRegister = func(fd int, va, size, off uint64) error {
		offsets = append(offsets, off)
		return handler.AddUffd(fd, va, size, off)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		srv.Stop()
		if err := loadResult(t, done); err != nil {
			t.Error(err)
		}
		if handler != nil {
			handler.Close()
		}
	}()
	first := loadPipe(t)
	defer unix.Close(first[0])
	if ack := sendTestVAReport(t, srv.Path, first[0], unitCHVA, PageSize); ack.Type != "error" {
		t.Fatalf("ack=%+v", ack)
	}
	if _, ok := m.Locate(unitCHVA); ok {
		t.Fatal("failed callback leaked staged map")
	}
	if ack := sendTestVAReport(t, srv.Path, first[0], unitCHVA, PageSize); ack.Type != "ack" {
		t.Fatalf("ack=%+v", ack)
	}
	next := loadPipe(t)
	defer unix.Close(next[0])
	if ack := sendTestVAReport(t, srv.Path, next[0], unitCHVA+0x100000, 2*PageSize); ack.Type != "ack" {
		t.Fatalf("ack=%+v", ack)
	}
	// Synchronize callback results through the server mutex, also under -race.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != PageSize || srv.nextMemfdOffset != 3*PageSize {
		t.Fatalf("offsets=%v next=%d", offsets, srv.nextMemfdOffset)
	}
	if _, fd, n, err := m.registration(PageSize, 2*PageSize); err != nil || fd <= 0 || n != 2*PageSize {
		t.Fatalf("registration fd=%d n=%d err=%v", fd, n, err)
	}
}

func TestVAReportStopInterruptsIncompleteHandshake(t *testing.T) {
	srv := &VAReportServer{Path: filepath.Join(t.TempDir(), "uffd.sock"), AddrMap: NewAddressMap(PageSize)}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: srv.Path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The peer sends no complete header. There is no configured handshake
	// timeout: lifetime cancellation must close the accepted socket itself.
	if _, err := conn.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := loadResult(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestVAReportFragmentedFramePreservesFD(t *testing.T) {
	for _, onBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "prefix-fd", true: "body-fd"}[onBody], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var pipe [2]int
			if err := unix.Pipe(pipe[:]); err != nil {
				t.Fatal(err)
			}
			defer unix.Close(pipe[0])
			defer unix.Close(pipe[1])
			body := []byte(`{"type":"va_report","version":1}`)
			var prefix [4]byte
			binary.LittleEndian.PutUint32(prefix[:], uint32(len(body)))
			sent := make(chan error, 1)
			go func() {
				c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
				if err != nil {
					sent <- err
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				rights := unix.UnixRights(pipe[0])
				if onBody {
					rights = nil
				}
				if _, _, err = c.WriteMsgUnix(prefix[:1], rights, nil); err != nil {
					sent <- err
					return
				}
				time.Sleep(10 * time.Millisecond)
				if _, err = c.Write(prefix[1:]); err != nil {
					sent <- err
					return
				}
				rights = nil
				if onBody {
					rights = unix.UnixRights(pipe[0])
				}
				_, _, err = c.WriteMsgUnix(body, rights, nil)
				sent <- err
			}()
			conn, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			got, fds, err := readLPJSONWithFDs(conn)
			defer closeAll(fds)
			if err != nil || string(got) != string(body) || len(fds) != 1 {
				t.Fatalf("frame=%s fds=%v err=%v", got, fds, err)
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			if _, err := unix.Write(pipe[1], []byte{9}); err != nil {
				t.Fatal(err)
			}
			var data [1]byte
			if _, err := unix.Read(fds[0], data[:]); err != nil || data[0] != 9 {
				t.Fatal("received descriptor invalid", err)
			}
		})
	}
}
