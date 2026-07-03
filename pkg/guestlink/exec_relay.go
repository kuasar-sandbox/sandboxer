package guestlink

import (
	"context"
	"io"
	"net"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// ServeExecRequest is the run-process side of `sandbox-ctl exec`. It
// runs on the ctl.sock server goroutine for one exec_request: open a
// fresh guest reverse-channel session (exec → exec_ack), relay the
// established stdio spec back to the CLI, then transparently pipe bytes
// both ways. The stdio MUX — including FrameExitStatus and the
// MUX_CLOSE handshake — runs end-to-end between `sandbox-ctl exec` and
// the guest; the run process is a dumb byte relay after the handshake.
func ServeExecRequest(ctx context.Context, conn net.Conn, req ctl.Request, vsockBase string, logf func(string, ...any)) {
	defer conn.Close()
	if req.Exec == nil || len(req.Exec.Argv) == 0 {
		_ = ctl.WriteMessage(conn, ctl.Response{Type: ctl.TypeError, Msg: "exec: empty argv"})
		return
	}
	hc := &HostClient{BasePath: vsockBase, Logf: logf}
	guestConn, established, err := OpenMUXViaExec(hc, req.Exec, proto.DeadlineExec)
	if err != nil {
		logf("exec: open guest session: %v", err)
		_ = ctl.WriteMessage(conn, ctl.Response{Type: ctl.TypeError, Msg: err.Error()})
		return
	}
	defer guestConn.Close()
	if err := ctl.WriteMessage(conn, ctl.Response{Type: ctl.TypeExecAck, Stdio: &established}); err != nil {
		logf("exec: write ack to ctl client: %v", err)
		return
	}
	pipeConns(ctx, conn, guestConn)
}

// pipeConns copies bytes both ways between a and b until either side
// closes (or ctx cancels), then closes both and waits for both copy
// goroutines to unwind. Each io.Copy returns exactly once, so the
// drain loop is bounded.
func pipeConns(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()

	got := 0
	select {
	case <-done:
		got++
	case <-ctx.Done():
	}
	_ = a.Close()
	_ = b.Close()
	for got < 2 {
		<-done
		got++
	}
}
