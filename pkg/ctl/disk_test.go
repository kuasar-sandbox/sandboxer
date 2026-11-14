package ctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"testing"
)

func TestDiskBindBodyExceedsControlFrameAndRejectsTruncation(t *testing.T) {
	// Pad the JSON with legal whitespace to exercise the bulk transfer without
	// testing this boundary through an enormous ref or filename.
	const disk = `{"slot_name":"target","disk":{"kind":"disk","version":1,"diff_template":"file:///trusted/seed"}}`
	body := strings.Repeat(" ", MaxMessageBytes) + disk
	for _, truncate := range []bool{false, true} {
		client, server := net.Pipe()
		called := false
		done := make(chan error, 1)
		s := Server{DiskBindHandler: func(req BindDiskRequest) error {
			called = true
			if req.SlotName != "target" {
				t.Errorf("received slot %q", req.SlotName)
			}
			return nil
		}}
		go func() {
			defer server.Close()
			done <- s.serveDisk(server, Request{Type: TypeDiskBind, BodyBytes: len(body)})
		}()
		if _, err := diskResponse(client, TypeDiskBindReady); err != nil {
			t.Fatal(err)
		}
		if truncate {
			if _, err := client.Write([]byte(body[:13])); err != nil {
				t.Fatal(err)
			}
			client.Close()
			if err := <-done; err == nil || called {
				t.Fatalf("truncated: called=%v err=%v", called, err)
			}
		} else {
			if _, err := client.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
			if _, err := diskResponse(client, TypeDiskBindDone); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil || !called {
				t.Fatalf("complete: called=%v err=%v", called, err)
			}
			client.Close()
		}
	}
}

func TestDiskInventoryPreservesDynamicUnknownCapacity(t *testing.T) {
	b, err := json.Marshal(DiskState{State: "unbound"})
	if err != nil || strings.Contains(string(b), "size_bytes") {
		t.Fatal(string(b), err)
	}
}

func TestDiskErrorCodeRoundTrip(t *testing.T) {
	for _, cause := range []error{
		ErrDiskInvalid, ErrDiskNotFound, ErrDiskUnsupported, ErrDiskConflict,
		ErrDiskCapacity, ErrDiskPending, ErrDiskBusy, fs.ErrPermission,
	} {
		client, server := net.Pipe()
		err := fmt.Errorf("bind work: %w", cause)
		done := make(chan error, 1)
		go func() {
			defer server.Close()
			done <- WriteMessage(server, diskWireError(err))
		}()
		_, received := diskResponse(client, TypeDiskBindDone)
		client.Close()
		if writeErr := <-done; writeErr != nil {
			t.Fatal(writeErr)
		}
		if !errors.Is(received, cause) || !strings.Contains(received.Error(), "bind work") {
			t.Fatalf("disk error %v: received %v", err, received)
		}
		if errors.Is(cause, fs.ErrPermission) && !errors.Is(received, ErrDiskPermission) {
			t.Fatalf("permission code lost: %v", received)
		}
	}
	if got := diskRemoteError(Response{Type: TypeError, Msg: "old peer"}); got.Error() != "old peer" {
		t.Fatal(got)
	}
}
