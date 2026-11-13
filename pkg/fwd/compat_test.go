package fwd_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	abi "github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/fwd"
	"github.com/kuasar-sandbox/sandboxer/pkg/fwd"
)

func TestCompatibilityFrames(t *testing.T) {
	want := abi.Frame{Type: abi.FrameData, Payload: []byte("migration-contract")}
	var legacy fwd.Frame = want
	var buf bytes.Buffer
	if err := fwd.WriteFrame(&buf, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := abi.ReadFrame(&buf)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility writer: got=%+v err=%v", got, err)
	}
	if err := abi.WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err = fwd.ReadFrame(&buf)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility reader: got=%+v err=%v", got, err)
	}
	err = fwd.WriteFrame(&buf, fwd.Frame{Payload: make([]byte, fwd.MaxFramePayload+1)})
	if !errors.Is(err, fwd.ErrFrameTooLarge) || !errors.Is(err, abi.ErrFrameTooLarge) {
		t.Fatalf("sentinel identity changed: %v", err)
	}
}
