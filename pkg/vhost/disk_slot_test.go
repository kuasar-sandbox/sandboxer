package vhost

import (
	"context"
	"errors"
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

func TestCapacityHandshakeWaitsForReverseRequestFD(t *testing.T) {
	server := NewServer("", &memBackend{}, nil)
	server.EnableCapacityChange()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { done <- server.WaitCapacityChange(ctx) }()
	set := func(features uint64) {
		t.Helper()
		if err := server.handleSetProtocolFeatures(nil, &Message{Payload: minimalProfileU64(features)}); err != nil {
			t.Fatal(err)
		}
	}
	set(bitBackendRequest | bitProtocolConfig)
	select {
	case err := <-done:
		t.Fatalf("completed before reverse fd arrived: %v", err)
	default:
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fds[0])
	msg := &Message{Header: Header{Request: MsgSetBackendReqFd}, Fds: []int{fds[1]}}
	if err := server.handle(nil, msg); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	server.resetConnectionState()
	set(bitProtocolConfig)
	if err := server.WaitCapacityChange(ctx); err == nil {
		t.Fatal("accepted legacy VMM")
	}
	server.resetConnectionState()
	set(bitProtocolConfig | bitBackendRequest)
	expired, stop := context.WithCancel(context.Background())
	stop()
	if err := server.WaitCapacityChange(expired); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBoundRestorePreservesAdvertisedCapacityFeatures(t *testing.T) {
	set := bindingSetForTest(t, BlockDeviceSpec{Slot: true, Capacity: 4096, CapacityChange: true}, BlockDeviceSpec{Slot: true, Capacity: 4096})
	devices := set.DeviceBackends()
	if !DynamicSlot(devices[0]) || DynamicSlot(devices[1]) || devices[0].Size() != 4096 {
		t.Fatal("dynamic protocol or visible device capacity changed")
	}
}
