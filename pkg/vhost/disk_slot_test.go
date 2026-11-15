package vhost

import (
	"encoding/binary"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestSlotPublicationIsAtomicAndLeavesOtherDevicesAlone(t *testing.T) {
	set := bindingSetForTest(t, BlockDeviceSpec{Capacity: 4096}, BlockDeviceSpec{Slot: true}, BlockDeviceSpec{Slot: true, Capacity: 8192})
	root := &memBackend{data: make([]byte, 4096)}
	if err := set.Bind([]Backend{root, nil, nil}); err != nil {
		t.Fatal(err)
	}
	devices := set.DeviceBackends()
	before := backendForRequest(devices[1])
	source := &memBackend{data: make([]byte, 8192)}
	durableFailure := errors.New("commit failed")
	if err := set.BindSlot(1, source, func() error { return durableFailure }); !errors.Is(err, durableFailure) {
		t.Fatal(err)
	}
	if devices[1].Size() != 0 || devices[2].Size() != 8192 {
		t.Fatal("publication after failed commit")
	}
	if err := set.BindSlot(1, source, nil); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(before.Flush(), ErrBackendUnbound) || devices[1].Size() != 8192 || devices[1].Flush() != nil || !errors.Is(devices[2].Flush(), ErrBackendUnbound) {
		t.Fatal("incorrect published or in-flight view")
	}
	if err := set.BindSlot(2, root, nil); err == nil {
		t.Fatal("fixed geometry changed")
	}
	if err := set.BindSlot(1, source, nil); err == nil {
		t.Fatal("bound twice")
	}
}

func TestSlotGetConfigWithoutCapacityNotifications(t *testing.T) {
	set := bindingSetForTest(t, BlockDeviceSpec{Slot: true})
	if err := set.Bind([]Backend{nil}); err != nil {
		t.Fatal(err)
	}
	server := NewServer("", set.DeviceBackends()[0], nil)
	server.EnableCapacityChange()
	if err := server.handleSetProtocolFeatures(nil, &Message{Payload: minimalProfileU64(bitProtocolConfig)}); err != nil {
		t.Fatal(err)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	peer := slotTestConn(t, fds[0])
	conn := slotTestConn(t, fds[1])
	checkCapacity := func(want uint64) {
		t.Helper()
		msg := &Message{Header: Header{Request: MsgGetConfig}, Payload: minimalProfileConfigRequest(0, 8, 0, 8)}
		if err := server.handle(conn, msg); err != nil {
			t.Fatal(err)
		}
		reply, err := ReadMessage(peer)
		if err != nil {
			t.Fatal(err)
		}
		if !reply.IsReply() || reply.Header.Request != MsgGetConfig || len(reply.Payload) != 20 {
			t.Fatalf("unexpected config reply: %+v", reply)
		}
		if got := binary.LittleEndian.Uint64(reply.Payload[12:]); got != want {
			t.Fatalf("GET_CONFIG returned %d sectors, want %d", got, want)
		}
	}
	checkCapacity(0)
	source := &memBackend{data: make([]byte, 8192)}
	if err := set.BindSlot(0, source, nil); err != nil {
		t.Fatal(err)
	}
	if err := server.NotifyCapacity(); err != nil {
		t.Fatal(err)
	}
	checkCapacity(uint64(source.Size()) / SectorSize)
}

func TestCapacityNotificationWhenNegotiated(t *testing.T) {
	server := NewServer("", &memBackend{}, nil)
	server.EnableCapacityChange()
	if err := server.handleSetProtocolFeatures(nil, &Message{Payload: minimalProfileU64(bitProtocolConfig | bitBackendRequest)}); err != nil {
		t.Fatal(err)
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	peer := slotTestConn(t, fds[0])
	if err := server.handle(nil, &Message{Header: Header{Request: MsgSetBackendReqFd}, Fds: []int{fds[1]}}); err != nil {
		t.Fatal(err)
	}
	defer server.resetConnectionState()
	if err := server.NotifyCapacity(); err != nil {
		t.Fatal(err)
	}
	msg, err := ReadMessage(peer)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Request != 2 || msg.Header.Flags != FlagVersion1 || msg.Header.Size != 0 {
		t.Fatalf("unexpected CONFIG_CHANGE_MSG header: %+v", msg.Header)
	}
}

func slotTestConn(t *testing.T, fd int) *net.UnixConn {
	t.Helper()
	file := os.NewFile(uintptr(fd), "vhost socketpair")
	conn, err := net.FileConn(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn.(*net.UnixConn)
}

func TestBoundRestorePreservesAdvertisedCapacityFeatures(t *testing.T) {
	set := bindingSetForTest(t, BlockDeviceSpec{Slot: true, Capacity: 4096, CapacityChange: true}, BlockDeviceSpec{Slot: true, Capacity: 4096})
	devices := set.DeviceBackends()
	if !DynamicSlot(devices[0]) || DynamicSlot(devices[1]) || devices[0].Size() != 4096 {
		t.Fatal("dynamic protocol or visible device capacity changed")
	}
}
