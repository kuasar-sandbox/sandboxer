package main

import (
	"net"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
)

func TestRunExecWaitsForGuestMUXCloseAfterExitStatus(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	defer hostConn.Close()
	defer guestConn.Close()

	hostDone := make(chan int, 1)
	go func() {
		hostDone <- runExecOverConn(
			hostConn,
			proto.ExecSpec{Argv: []string{"/bin/true"}},
			stdio.Mode{},
		)
	}()

	var request ctl.Request
	if err := ctl.ReadMessage(guestConn, &request); err != nil {
		t.Fatal(err)
	}
	if request.Type != ctl.TypeExecRequest || request.Exec == nil {
		t.Fatalf("request = %+v", request)
	}
	if err := ctl.WriteMessage(guestConn, &ctl.Response{
		Type:  ctl.TypeExecAck,
		Stdio: &request.Exec.Stdio,
	}); err != nil {
		t.Fatal(err)
	}

	guestSession := mux.NewSession(guestConn, mux.StreamSet{}, mux.Options{})
	defer guestSession.Close()
	if err := guestSession.SendExitStatus(37); err != nil {
		t.Fatal(err)
	}

	select {
	case code := <-hostDone:
		t.Fatalf("exec returned code %d before the guest MUX_CLOSE barrier", code)
	case <-time.After(100 * time.Millisecond):
	}

	if err := guestSession.InitMuxClose(); err != nil {
		t.Fatalf("guest MUX close: %v", err)
	}
	select {
	case code := <-hostDone:
		if code != 37 {
			t.Fatalf("exec code = %d, want 37", code)
		}
	case <-time.After(time.Second):
		t.Fatal("exec did not return after the guest MUX_CLOSE barrier")
	}
}
