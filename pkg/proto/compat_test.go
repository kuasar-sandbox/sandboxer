package proto_test

import (
	"bytes"
	"reflect"
	"testing"

	abi "github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestCompatibilityMessages(t *testing.T) {
	want := &abi.Message{Type: abi.TypeUsageRequest, UsageRequest: &abi.UsageRequest{
		RunEpoch: "migration-contract", RequestID: 42, ReadBudgetNS: 1000000,
	}}
	// Assignment is also a compile-time check that the old type is an alias.
	var legacy *proto.Message = want
	var buf bytes.Buffer
	if err := proto.WriteMessage(&buf, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := abi.ReadMessage(&buf)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility writer: got=%+v err=%v", got, err)
	}
	if err := abi.WriteMessage(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err = proto.ReadMessage(&buf)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility reader: got=%+v err=%v", got, err)
	}
	if !bytes.Equal(proto.HostConnectLine, abi.HostConnectLine) {
		t.Fatal("host connection prefix changed")
	}
}
