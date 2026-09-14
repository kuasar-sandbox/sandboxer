package ctl

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeReadCancellationClosesConnectedSocket(t *testing.T) {
	for _, resource := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		path := filepath.Join(t.TempDir(), "ctl.sock")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		accepted, closed := make(chan struct{}), make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				closed <- err
				return
			}
			defer conn.Close()
			var req Request
			if err := ReadMessage(conn, &req); err != nil {
				closed <- err
				return
			}
			close(accepted)
			var b [1]byte
			_, err = conn.Read(b[:])
			closed <- err
		}()
		result := make(chan error, 1)
		go func() {
			if resource {
				_, err := ReadResourceStats(ctx, path, "sid")
				result <- err
			} else {
				_, connected, err := ReadUsage(ctx, path, false, 0, 1)
				if !connected {
					err = errors.New("lost connected owner after cancellation")
				}
				result <- err
			}
		}()
		select {
		case <-accepted:
		case <-time.After(3 * time.Second):
			t.Fatal("native request did not reach owner")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("native read ignored cancellation without deadline")
		}
		if err := <-closed; err != io.EOF {
			t.Fatal("canceled socket was not closed", err)
		}
	}
}

func TestResourceStatsDispatchAndIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.sock")
	value := uint64(9007199254740993)
	server := &Server{Path: path, SnapshotHandler: func(Request) (Response, error) {
		t.Error("resource read triggered snapshot")
		return Response{}, errors.New("unexpected snapshot")
	}, ResourceStatsHandler: func(Request) (Response, error) {
		return Response{ResourceStats: &ResourceStats{SandboxID: "sid", MemoryUsed: &value, CPUUsageUsec: &value}}, nil
	}}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	got, err := ReadResourceStats(ctx, path, "sid")
	if err != nil || got.CPUUsageUsec == nil || *got.CPUUsageUsec != value || got.MemoryUsed == nil || *got.MemoryUsed != value {
		t.Fatalf("lossy native snapshot: %+v %v", got, err)
	}
	if _, err := ReadResourceStats(ctx, path, "stable-alias"); err == nil {
		t.Fatal("different SandboxID was accepted")
	}
}
