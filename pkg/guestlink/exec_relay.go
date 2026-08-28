package guestlink

import (
	"context"
	"io"
	"net"
	"sync"

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

// pipeConns copies bytes both ways between a and b. A read EOF in one
// direction is propagated as a write-side half-close to the peer so the
// reverse direction can still deliver trailing protocol frames such as
// exec exit status. Context cancellation or a copy error fully closes both
// transports; otherwise the relay closes them after both directions drain.
func pipeConns(ctx context.Context, a, b net.Conn) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = a.Close()
			_ = b.Close()
		})
	}
	closeWrite := func(c net.Conn) {
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
			return
		}
		_ = c.Close()
	}

	done := make(chan struct{}, 2)
	copyOne := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err != nil {
			// A local full close (capture/shutdown) surfaces as a copy error,
			// unlike a peer's graceful EOF. Close both transports so a silent
			// reverse exec session cannot strand the capture drain.
			closeBoth()
		} else {
			closeWrite(dst)
		}
		done <- struct{}{}
	}
	go copyOne(a, b)
	go copyOne(b, a)

	for got := 0; got < 2; got++ {
		select {
		case <-done:
		case <-ctx.Done():
			closeBoth()
			for got < 2 {
				<-done
				got++
			}
			return
		}
	}
	closeBoth()
}
