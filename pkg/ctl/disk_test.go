package ctl

import (
	"encoding/json"
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
