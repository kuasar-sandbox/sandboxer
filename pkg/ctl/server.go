package ctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// Server runs the ctl.sock listener inside a sandbox-ctl run process.
// It dispatches snapshot_request (one-shot JSON) and exec_request
// (handshake then a long-lived stdio-MUX pipe) to the configured
// handlers. Each accepted connection is serviced in its own goroutine,
// so concurrent snapshot/exec requests don't block each other.
type Server struct {
	Path string

	// SnapshotHandler services a snapshot_request: it returns the
	// response and the server writes it back, then closes the conn.
	SnapshotHandler      func(req Request) (Response, error)
	ExportHandler        func(req Request) (Response, error)
	UsageHandler         func(req Request) (Response, error)
	ResourceStatsHandler func(req Request) (Response, error)

	// ExecHandler services an exec_request. It takes ownership of conn
	// (including its lifetime): it writes the ctl exec_ack / error
	// response itself, then pipes the stdio MUX, then closes conn. nil
	// → exec_request is rejected.
	ExecHandler func(conn net.Conn, req Request)

	Logf func(string, ...any)

	listener *net.UnixListener
	stopOnce sync.Once
	stopped  chan struct{}
}

// Listen binds the UDS.
func (s *Server) Listen() error {
	if s.SnapshotHandler == nil {
		return errors.New("ctl: Server.SnapshotHandler is nil")
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	s.stopped = make(chan struct{})
	_ = os.Remove(s.Path)
	addr, err := net.ResolveUnixAddr("unix", s.Path)
	if err != nil {
		return fmt.Errorf("ctl.sock resolve: %w", err)
	}
	l, err := net.ListenUnix("unix", addr)
	if err != nil {
		return fmt.Errorf("ctl.sock listen: %w", err)
	}
	s.listener = l
	return nil
}

// Serve accepts connections until ctx cancels or Stop is called.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("ctl: Listen not called")
	}
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			select {
			case <-s.stopped:
				return nil
			default:
				return fmt.Errorf("ctl.sock accept: %w", err)
			}
		}
		go s.handle(conn)
	}
}

// Stop closes the listener and removes the socket file.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopped)
		if s.listener != nil {
			_ = s.listener.Close()
		}
		_ = os.Remove(s.Path)
	})
}

func (s *Server) handle(conn *net.UnixConn) {
	var req Request
	if err := ReadMessage(conn, &req); err != nil {
		s.Logf("ctl.sock: read: %v", err)
		_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
		_ = conn.Close()
		return
	}

	switch req.Type {
	case TypeResourceStatsRequest:
		defer conn.Close()
		if s.ResourceStatsHandler == nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: "resource stats unavailable"})
			return
		}
		resp, err := s.ResourceStatsHandler(req)
		if err != nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
			return
		}
		resp.Type = TypeResourceStatsResponse
		if err := WriteMessage(conn, resp); err != nil {
			s.Logf("ctl.sock resource stats response: %v", err)
		}
		return
	case TypeUsageRequest:
		defer conn.Close()
		if s.UsageHandler == nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: "usage unavailable"})
			return
		}
		resp, err := s.UsageHandler(req)
		if err != nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
			return
		}
		resp.Type = TypeUsageResponse
		if err := WriteUsageResponse(conn, resp); err != nil {
			s.Logf("ctl.sock usage response: %v", err)
		}
		return
	case TypeExecRequest:
		if s.ExecHandler == nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: "exec not supported"})
			_ = conn.Close()
			return
		}
		// ExecHandler owns conn (writes its own response, pipes the MUX,
		// closes conn) — it is long-lived, so it must NOT be closed here.
		s.ExecHandler(conn, req)
		return

	case TypeSnapshotRequest:
		defer conn.Close()
		resp, err := s.SnapshotHandler(req)
		if err != nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
			return
		}
		resp.Type = TypeSnapshotDone
		if err := WriteMessage(conn, resp); err != nil {
			s.Logf("ctl.sock: write resp: %v", err)
		}
		return

	case TypeExportRequest:
		defer conn.Close()
		if s.ExportHandler == nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: "export not supported"})
			return
		}
		resp, err := s.ExportHandler(req)
		if err != nil {
			_ = WriteMessage(conn, Response{Type: TypeError, Msg: err.Error()})
			return
		}
		resp.Type = TypeExportDone
		if err := WriteMessage(conn, resp); err != nil {
			s.Logf("ctl.sock: write export resp: %v", err)
		}
		return

	default:
		_ = WriteMessage(conn, Response{Type: TypeError, Msg: "unknown type: " + req.Type})
		_ = conn.Close()
		return
	}
}
