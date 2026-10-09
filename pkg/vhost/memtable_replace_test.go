package vhost

import (
	"context"
	"encoding/binary"
	"net"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSetMemTablePreservesOperatingQueue(t *testing.T) {
	const uva = uint64(0x10000)
	fd, err := unix.MemfdCreate("memtable-replace", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, 8192); err != nil {
		t.Fatal(err)
	}
	slab, err := unix.Mmap(fd, 0, 8192, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(slab)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		t.Fatal(err)
	}
	s := NewServer(filepath.Join(t.TempDir(), "vhost.sock"), &memBackend{data: make([]byte, 512)}, nil)
	s.SetMemfd(stat.Ino, slab)
	s.SetMemoryLoader(initializedTestMemory)
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	defer func() {
		cancel()
		s.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("Serve did not stop")
		}
	}()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: s.socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	send := func(req uint32, payload []byte, fds ...int) {
		t.Helper()
		hdr := make([]byte, HeaderSize)
		binary.LittleEndian.PutUint32(hdr, req)
		binary.LittleEndian.PutUint32(hdr[4:], FlagVersion1)
		binary.LittleEndian.PutUint32(hdr[8:], uint32(len(payload)))
		var rights []byte
		if len(fds) != 0 {
			rights = syscall.UnixRights(fds...)
		}
		if _, _, err := conn.WriteMsgUnix(hdr, rights, nil); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 0 {
			if _, err := conn.Write(payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	barrier := func() {
		t.Helper()
		send(MsgGetFeatures, nil)
		if _, err := ReadMessage(conn); err != nil {
			t.Fatal(err)
		}
	}
	table := func(offset uint64) {
		p := make([]byte, 40)
		binary.LittleEndian.PutUint32(p, 1)
		binary.LittleEndian.PutUint64(p[16:], 4096)
		binary.LittleEndian.PutUint64(p[24:], uva)
		binary.LittleEndian.PutUint64(p[32:], offset)
		send(MsgSetMemTable, p, fd)
		barrier()
	}
	state := func(req, value uint32) {
		p := make([]byte, 8)
		binary.LittleEndian.PutUint32(p[4:], value)
		send(req, p)
	}
	event := func() int {
		fd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unix.Close(fd) })
		return fd
	}
	kick, call := event(), event()
	wake := func() {
		t.Helper()
		if _, err := unix.Write(kick, []byte{1, 0, 0, 0, 0, 0, 0, 0}); err != nil {
			t.Fatal(err)
		}
	}
	completion := func() {
		t.Helper()
		p := []unix.PollFd{{Fd: int32(call), Events: unix.POLLIN}}
		if n, err := unix.Poll(p, 2000); err != nil || n != 1 {
			t.Fatalf("call notification: n=%d err=%v", n, err)
		}
		var b [8]byte
		if _, err := unix.Read(call, b[:]); err != nil {
			t.Fatal(err)
		}
	}
	features := make([]byte, 8)
	binary.LittleEndian.PutUint64(features, bitVhostProtocolFeatures)
	send(MsgSetFeatures, features)
	table(0)
	mem := slab[:4096]
	// Two descriptors form a FLUSH request, with an independently writable status.
	binary.LittleEndian.PutUint64(mem, 256)
	binary.LittleEndian.PutUint32(mem[8:], 16)
	binary.LittleEndian.PutUint16(mem[12:], descFlagNext)
	binary.LittleEndian.PutUint16(mem[14:], 1)
	binary.LittleEndian.PutUint64(mem[16:], 512)
	binary.LittleEndian.PutUint32(mem[24:], 1)
	binary.LittleEndian.PutUint16(mem[28:], descFlagWrite)
	binary.LittleEndian.PutUint32(mem[256:], BlkTypeFlush)
	mem[512] = 0xff
	binary.LittleEndian.PutUint16(mem[130:], 1)
	state(MsgSetVringNum, 8)
	state(MsgSetVringBase, 0)
	addr := make([]byte, 40)
	binary.LittleEndian.PutUint64(addr[8:], uva)
	binary.LittleEndian.PutUint64(addr[16:], uva+192)
	binary.LittleEndian.PutUint64(addr[24:], uva+128)
	send(MsgSetVringAddr, addr)
	send(MsgSetVringCall, make([]byte, 8), call)
	send(MsgSetVringKick, make([]byte, 8), kick)
	state(MsgSetVringEnable, 1)
	barrier()
	wake()
	completion()
	s.Quiesce()
	paused := true
	defer func() {
		if paused {
			s.Resume()
		}
	}()
	s.mu.Lock()
	q := s.queues[0]
	s.mu.Unlock()
	q.drainMu.Lock()
	oldKick, oldCall, oldDone := q.kickFd, q.callFd, q.done
	q.drainMu.Unlock()
	check := func(mem []byte, want uint16) {
		t.Helper()
		if got := binary.LittleEndian.Uint16(mem[194:]); got != want || mem[512] != BlkStatusOK || q.baseIdx != want {
			t.Fatalf("completion: used=%d status=%d base=%d want=%d", got, mem[512], q.baseIdx, want)
		}
		entry := 196 + int((want-1)%8)*8
		if id, n := binary.LittleEndian.Uint32(mem[entry:]), binary.LittleEndian.Uint32(mem[entry+4:]); id != 0 || n != 1 {
			t.Fatalf("used entry: id=%d len=%d", id, n)
		}
	}
	check(mem, 1)
	// Move the same guest addresses to a different slab offset. Queue progress
	// is copied like guest RAM, but no SET_VRING message accompanies replacement.
	next := slab[4096:]
	copy(next, mem)
	next[512] = 0xff
	binary.LittleEndian.PutUint16(next[130:], 2)
	wake() // May be consumed by the old worker while it waits at the pause gate.
	table(4096)
	select {
	case <-oldDone:
	default:
		t.Fatal("replacement did not join old worker")
	}
	q.drainMu.Lock()
	s.mu.Lock()
	retained := s.queues[0] == q && q.num == 8 && q.descAddr == uva && q.availAddr == uva+128 && q.usedAddr == uva+192 && q.baseIdx == 1 && q.enabled && q.kickFd == oldKick && q.callFd == oldCall
	s.mu.Unlock()
	q.drainMu.Unlock()
	if !retained {
		t.Fatal("replacement lost queue configuration, descriptors, progress or enable state")
	}
	s.Resume()
	paused = false
	completion() // Pending work must survive even if the old worker ate its kick.
	s.Quiesce()
	paused = true
	check(next, 2)
	checkOld := binary.LittleEndian.Uint16(mem[194:])
	if checkOld != 1 {
		t.Fatalf("old translation used: used=%d", checkOld)
	}
	next[512] = 0xff
	binary.LittleEndian.PutUint16(next[130:], 3)
	s.Resume()
	paused = false
	wake() // The original kick/call pair still operates after replacement.
	completion()
	s.Quiesce()
	paused = true
	check(next, 3)
	// A disabled queue remains configured but is not reactivated by a table swap.
	state(MsgSetVringEnable, 0)
	barrier()
	table(4096)
	q.drainMu.Lock()
	s.mu.Lock()
	retained = s.queues[0] == q && !q.enabled && q.stop == nil && q.baseIdx == 3 && q.kickFd == oldKick && q.callFd == oldCall
	s.mu.Unlock()
	q.drainMu.Unlock()
	if !retained {
		t.Fatal("disabled queue state was not retained")
	}
}
