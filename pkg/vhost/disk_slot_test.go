package vhost

import (
	"errors"
	"testing"
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
