package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/mux"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/stdio"
)

func TestExecCmdUsesPathIDWithoutSandboxIDAndWithPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		sandboxID string
	}{
		{name: "path id only"},
		{name: "path id takes precedence", sandboxID: "logical-sandbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runRoot, err := os.MkdirTemp("", "pathid-exec-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
			pathID := "phase-b"
			runDir := filepath.Join(runRoot, pathID)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(runDir, "ctl.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			serverDone := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				var req ctl.Request
				if err := ctl.ReadMessage(conn, &req); err != nil {
					serverDone <- err
					return
				}
				if req.Type != ctl.TypeExecRequest || req.Exec == nil {
					serverDone <- &unexpectedExecRequestError{request: req}
					return
				}
				if err := ctl.WriteMessage(conn, &ctl.Response{Type: ctl.TypeExecAck, Stdio: &req.Exec.Stdio}); err != nil {
					serverDone <- err
					return
				}
				session := mux.NewSession(conn, mux.StreamSet{}, mux.Options{})
				defer session.Close()
				if err := session.SendExitStatus(0); err != nil {
					serverDone <- err
					return
				}
				serverDone <- session.InitMuxClose()
			}()

			args := []string{"--path-id", pathID, "--run-root", runRoot, "--stdout=false", "--stderr=false", "--", "/bin/true"}
			if test.sandboxID != "" {
				args = append([]string{"--sandbox-id", test.sandboxID}, args...)
			}
			if code := execCmd(args); code != 0 {
				t.Fatalf("execCmd exit=%d", code)
			}
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

type unexpectedExecRequestError struct {
	request ctl.Request
}

func (e *unexpectedExecRequestError) Error() string {
	return "unexpected exec request type " + e.request.Type
}

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
