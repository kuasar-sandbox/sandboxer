package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

func TestMemReportNotifyDeadlineFitsRetryInterval(t *testing.T) {
	if memReportNotifyDeadline <= 0 {
		t.Fatalf("memReportNotifyDeadline = %v, want positive", memReportNotifyDeadline)
	}
	if memReportNotifyDeadline >= memReportInterval {
		t.Fatalf("memReportNotifyDeadline = %v, want less than interval %v", memReportNotifyDeadline, memReportInterval)
	}
}

func TestExchangeMemReportAllowsPressureDelayedAck(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverDone := make(chan error, 1)
	go func() {
		req, err := proto.ReadMessage(server)
		if err != nil {
			serverDone <- fmt.Errorf("read report: %w", err)
			return
		}
		if req.Type != proto.TypeMemReport {
			serverDone <- fmt.Errorf("message type = %q, want %q", req.Type, proto.TypeMemReport)
			return
		}
		time.Sleep(proto.DeadlineAppNotify + 50*time.Millisecond)
		serverDone <- proto.WriteMessage(server, &proto.Message{Type: proto.TypeMemReportAck})
	}()

	if err := exchangeMemReport(client, 128<<20, 256<<20, memReportNotifyDeadline); err != nil {
		t.Fatalf("exchangeMemReport with pressure-delayed ACK: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestExchangeMemReportBoundsTrickledAck(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	client := &vsockConn{fd: fds[0]}
	server := &vsockConn{fd: fds[1]}
	defer client.Close()
	defer server.Close()

	serverDone := make(chan error, 1)
	go func() {
		req, err := proto.ReadMessage(server)
		if err != nil {
			serverDone <- fmt.Errorf("read report: %w", err)
			return
		}
		if req.Type != proto.TypeMemReport {
			serverDone <- fmt.Errorf("message type = %q, want %q", req.Type, proto.TypeMemReport)
			return
		}

		var ack bytes.Buffer
		if err := proto.WriteMessage(&ack, &proto.Message{Type: proto.TypeMemReportAck}); err != nil {
			serverDone <- fmt.Errorf("encode ACK: %w", err)
			return
		}
		for _, b := range ack.Bytes() {
			time.Sleep(15 * time.Millisecond)
			if _, err := server.Write([]byte{b}); err != nil {
				serverDone <- nil
				return
			}
		}
		serverDone <- errors.New("trickled ACK completed beyond the exchange budget")
	}()

	const budget = 100 * time.Millisecond
	started := time.Now()
	if err := exchangeMemReport(client, 128<<20, 256<<20, budget); err == nil {
		t.Fatal("exchangeMemReport accepted an ACK that exceeded the wall-clock budget")
	}
	if elapsed := time.Since(started); elapsed > 3*budget {
		t.Fatalf("exchangeMemReport returned after %v, want within %v", elapsed, 3*budget)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("trickled ACK server did not observe deadline close")
	}
}

func TestMemReportAttemptLogsRecoveryAfterTransientFailures(t *testing.T) {
	var logs []string
	notifyCalls := 0
	push := newMemReportAttempt(
		func() (uint64, uint64, error) { return 128 << 20, 256 << 20, nil },
		func(avail, total uint64) error {
			notifyCalls++
			if avail != 128<<20 || total != 256<<20 {
				t.Fatalf("notify(%d, %d)", avail, total)
			}
			if notifyCalls <= 2 {
				return errors.New("read: resource temporarily unavailable")
			}
			return nil
		},
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	)

	push()
	push()
	push()
	push()

	want := []string{
		"mem_report: read: resource temporarily unavailable",
		"mem_report: read: resource temporarily unavailable",
		"mem_report: recovered after 2 consecutive failures",
	}
	if !reflect.DeepEqual(logs, want) {
		t.Fatalf("logs = %#v, want %#v", logs, want)
	}
}
