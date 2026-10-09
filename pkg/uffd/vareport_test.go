package uffd

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

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
