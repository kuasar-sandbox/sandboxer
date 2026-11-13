package mux_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	abi "github.com/kuasar-sandbox/guest-runtime/pkg/guestabi/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
)

func TestCompatibilityFrames(t *testing.T) {
	want := abi.Frame{Stream: abi.StreamStdout, Type: abi.FrameData, Payload: []byte("migration-contract")}
	var legacy mux.Frame = want
	var buf bytes.Buffer
	if err := mux.WriteFrame(&buf, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := abi.ReadFrame(&buf)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility writer: got=%+v err=%v", got, err)
	}
	if err := abi.WriteFrame(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err = mux.ReadFrame(&buf)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("compatibility reader: got=%+v err=%v", got, err)
	}
	err = mux.WriteFrame(&buf, mux.Frame{Payload: make([]byte, mux.MaxFramePayload+1)})
	if !errors.Is(err, mux.ErrFrameTooLarge) || !errors.Is(err, abi.ErrFrameTooLarge) {
		t.Fatalf("sentinel identity changed: %v", err)
	}
	if mux.ErrClosed != abi.ErrClosed || mux.ErrProtocol != abi.ErrProtocol || mux.ErrStreamReset != abi.ErrStreamReset {
		t.Fatal("session sentinel identity changed")
	}
}
