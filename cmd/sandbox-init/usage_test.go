package main

import (
	"context"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

func TestUsageMemoryPCPAndDomains(t *testing.T) {
	zone := []byte("Node 0, zone Normal\n pages free 20\n present 100\n spanned 900\n pagesets\n cpu: 0\n count: 3\n cpu: 1\n count: 4\nNode 0, zone Device\n present 999\nNode 0, zone Movable\n")
	buddy := []byte("Node 0, zone Normal 1 2 3\n")
	m, err := parseUsageMemory(zone, buddy, 4096)
	if err != nil || *m.PresentPages != 100 || *m.BuddyFreePages != 17 || *m.PCPFreePages != 7 || *m.PageSize != 4096 {
		t.Fatalf("%+v %v", m, err)
	}
	for _, bad := range []string{strings.Replace(string(zone), "count: 3", "missing: 3", 1), strings.Replace(string(zone), "present 100", "spanned 100", 1), strings.Replace(string(zone), "cpu: 1", "cpu: 0", 1), strings.Replace(string(zone), "Normal", "HighMem", 1)} {
		if _, err := parseUsageMemory([]byte(bad), buddy, 4096); err == nil {
			t.Fatal(bad)
		}
	}
	if _, err := parseUsageMemory(zone, []byte("Node 1, zone Normal 1"), 4096); err == nil {
		t.Fatal("accepted different node")
	}
	// This checks parser integration on the host kernel, not target Guest/KVM
	// acceptance. The latter is exercised by the component E2E suite.
	if _, err := readUsageMemory(); err != nil {
		t.Fatal(err)
	}
}

func TestUsageFirstBlockPersistentBusyAndNewData(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	var starts atomic.Int32
	s := newUsageService()
	s.sources[0].read = func() usageRawResult {
		return usageRawResult{memory: proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}
	}
	s.sources = append(s.sources, &usageSource{disk: "blocked", read: func() usageRawResult {
		starts.Add(1)
		close(blocked)
		<-release
		return usageRawResult{filesystem: proto.UsageFilesystem{Disk: "blocked", UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}
	}})
	var healthy atomic.Uint64
	s.sources = append(s.sources, &usageSource{disk: "healthy", read: func() usageRawResult {
		return usageRawResult{filesystem: proto.UsageFilesystem{Disk: "healthy", Blocks: usageUint(healthy.Add(1)), UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}
	}})
	r := s.collect(proto.UsageRequest{RunEpoch: "one", RequestID: 1, ReadBudgetNS: int64(20 * time.Millisecond)}, nil)
	<-blocked
	if r.Memory.Status != proto.UsageOK || r.Filesystems[0].Status != proto.UsageTimeout || r.Filesystems[1].Status != proto.UsageOK {
		t.Fatalf("first block: %+v", r)
	}
	baseline := runtime.NumGoroutine()
	for i := 2; i < 102; i++ {
		r = s.collect(proto.UsageRequest{RunEpoch: "one", RequestID: uint64(i), ReadBudgetNS: int64(time.Second)}, nil)
		if r.Filesystems[0].Status != proto.UsageBusy || r.Memory.Status != proto.UsageOK || r.Filesystems[1].Status != proto.UsageOK {
			t.Fatalf("ongoing busy: %+v", r)
		}
	}
	if starts.Load() != 1 || healthy.Load() != 101 || runtime.NumGoroutine() > baseline+3 {
		t.Fatal("source created replacement workers")
	}
	close(release)
}

func TestUsageConnectionEpochAndQuiesce(t *testing.T) {
	s := newUsageService()
	s.sources[0].read = func() usageRawResult {
		return usageRawResult{memory: proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageOK}}}
	}
	connect := func(epoch string, id uint64) (*os.File, <-chan struct{}) {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		client := os.NewFile(uintptr(pair[1]), "usage-client")
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.serve(&vsockConn{fd: pair[0]}, &proto.Message{Type: proto.TypeUsageRequest, UsageRequest: &proto.UsageRequest{RunEpoch: epoch, RequestID: id, ReadBudgetNS: int64(100 * time.Millisecond)}})
		}()
		return client, done
	}
	c, done := connect("one", 1)
	msg, err := proto.ReadMessage(c)
	if err != nil || msg.UsageResponse.RequestID != 1 {
		t.Fatalf("%v %+v", err, msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.pause(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("usage connection survived quiesce")
	}
	c.Close()
	s.resume(true)
	c, done = connect("two", 1)
	msg, err = proto.ReadMessage(c)
	if err != nil || msg.UsageResponse.RunEpoch != "two" {
		t.Fatalf("%v %+v", err, msg)
	}
	c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("idle EOF did not release connection")
	}
}

func FuzzUsageMemory(f *testing.F) {
	f.Add([]byte("Node 0, zone Normal\n"), []byte("Node 0, zone Normal 0\n"))
	f.Fuzz(func(t *testing.T, z, b []byte) {
		if len(z)+len(b) > 2*1024*1024 {
			return
		}
		_, _ = parseUsageMemory(z, b, 4096)
	})
}
