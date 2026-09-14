package ctl

import (
	"context"
	"errors"
	"fmt"
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
	stamp := time.Now().Unix()
	server := &Server{Path: path, SnapshotHandler: func(Request) (Response, error) {
		t.Error("resource read triggered snapshot")
		return Response{}, errors.New("unexpected snapshot")
	}, ResourceStatsHandler: func(Request) (Response, error) {
		return Response{ResourceStats: &ResourceStats{SandboxID: "sid", CPUCapacity: 2, CPUAllocatable: .5,
			MemoryCapacity: 4096, MemoryHeadroom: 1024, MemoryUsed: &value, CPUUsageUsec: &value, TimestampUnix: &stamp}}, nil
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

func TestResourceStatsRequiredSpecification(t *testing.T) {
	dir := t.TempDir()
	for i, test := range []struct {
		name   string
		change func(*ResourceStats)
		valid  bool
	}{
		{"valid-host-zero", func(*ResourceStats) {}, true},
		{"missing-spec", func(s *ResourceStats) { *s = ResourceStats{SandboxID: "sid"} }, false},
		{"cpu-zero", func(s *ResourceStats) { s.CPUCapacity = 0 }, false},
		{"cpu-negative", func(s *ResourceStats) { s.CPUCapacity = -1 }, false},
		{"allocatable-zero", func(s *ResourceStats) { s.CPUAllocatable = 0 }, false},
		{"allocatable-negative", func(s *ResourceStats) { s.CPUAllocatable = -.5 }, false},
		{"allocatable-over", func(s *ResourceStats) { s.CPUAllocatable = 3 }, false},
		{"memory-zero", func(s *ResourceStats) { s.MemoryCapacity = 0 }, false},
		{"headroom-zero", func(s *ResourceStats) { s.MemoryHeadroom = 0 }, false},
		{"headroom-over", func(s *ResourceStats) { s.MemoryHeadroom = 4097 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			zero := uint64(0)
			stamp := time.Now().Unix()
			stats := ResourceStats{SandboxID: "sid", CPUCapacity: 2, CPUAllocatable: .5,
				MemoryCapacity: 4096, MemoryHeadroom: 1024, MemoryUsed: &zero, CPUUsageUsec: &zero, TimestampUnix: &stamp}
			test.change(&stats)
			server := &Server{Path: filepath.Join(dir, fmt.Sprintf("%d.sock", i)), SnapshotHandler: func(Request) (Response, error) {
				t.Error("resource read triggered snapshot")
				return Response{}, errors.New("unexpected snapshot")
			}, ResourceStatsHandler: func(Request) (Response, error) {
				return Response{ResourceStats: &stats}, nil
			}}
			if err := server.Listen(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			got, err := ReadResourceStats(ctx, server.Path, "sid")
			if !test.valid {
				if err == nil {
					t.Fatalf("invalid required spec accepted: %+v", got)
				}
				return
			}
			if err != nil || got.MemoryUsed == nil || *got.MemoryUsed != 0 || got.CPUUsageUsec == nil || *got.CPUUsageUsec != 0 || got.CPUAllocatable != .5 {
				t.Fatalf("valid specification/zero host counters lost: %+v, %v", got, err)
			}
		})
	}
}

func TestResourceStatsHostObservationShape(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprint(mask), func(t *testing.T) {
			zero, stamp := uint64(0), time.Now().Unix()
			stats := ResourceStats{SandboxID: "sid", CPUCapacity: 2, CPUAllocatable: .5, MemoryCapacity: 4096, MemoryHeadroom: 1024}
			if mask&1 != 0 {
				stats.MemoryUsed = &zero
			}
			if mask&2 != 0 {
				stats.CPUUsageUsec = &zero
			}
			if mask&4 != 0 {
				stats.TimestampUnix = &stamp
			}
			server := &Server{Path: filepath.Join(t.TempDir(), "ctl.sock"), SnapshotHandler: func(Request) (Response, error) {
				t.Error("resource read triggered snapshot")
				return Response{}, errors.New("unexpected snapshot")
			}, ResourceStatsHandler: func(Request) (Response, error) { return Response{ResourceStats: &stats}, nil }}
			if err := server.Listen(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- server.Serve(ctx) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			got, err := ReadResourceStats(ctx, server.Path, "sid")
			valid := (mask&4 != 0) == (mask&3 != 0)
			if !valid {
				if err == nil {
					t.Fatal("invalid host shape accepted", mask, got)
				}
				return
			}
			if err != nil || (got.MemoryUsed != nil) != (mask&1 != 0) || (got.CPUUsageUsec != nil) != (mask&2 != 0) || (got.TimestampUnix != nil) != (mask&4 != 0) {
				t.Fatal("valid independent observation lost", mask, got, err)
			}
		})
	}
}
