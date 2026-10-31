package ctl

import (
	"context"
	"errors"
	"net"
	"time"
)

// dialContext shares connection cancellation and deadlines, not request framing
// or retry policy. Only transport errors are translated: a complete reply still
// determines the operation's result, including any server-side error.
func dialContext(ctx context.Context, path string) (net.Conn, func(), error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	close := func() { stop(); _ = conn.Close() }
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			close()
			return nil, nil, contextTransportError(ctx, err)
		}
	}
	return &contextConn{Conn: conn, ctx: ctx}, close, nil
}

type contextConn struct {
	net.Conn
	ctx context.Context
}

func (c *contextConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, contextTransportError(c.ctx, err)
}

func (c *contextConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return n, contextTransportError(c.ctx, err)
}

func contextTransportError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) && ctx.Err() != nil {
		return ctx.Err()
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The socket deadline can fire before the context's timer callback.
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return err
}
