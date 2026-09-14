package ctl

import (
	"context"
	"errors"
	"net"
)

// dialStats shares only the cancellation/deadline handling of the two native
// read operations. Requests and their framing remain specific to each reader.
func dialStats(ctx context.Context, path string) (net.Conn, func(), error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	close := func() { stop(); _ = conn.Close() }
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			close()
			return nil, nil, err
		}
	}
	return conn, close, nil
}

// ReadUsage reports whether the owner was connected so callers cannot silently
// replace an online error with a read of its unaccepted file tail.
func ReadUsage(ctx context.Context, socket string, history bool, cursor int64, limit int) (response Response, connected bool, err error) {
	conn, close, err := dialStats(ctx, socket)
	if err != nil {
		return Response{}, false, err
	}
	defer close()
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if err := WriteMessage(conn, Request{Type: TypeUsageRequest, UsageHistory: history, UsageCursor: cursor, UsageLimit: limit}); err != nil {
		return Response{}, true, err
	}
	response, err = ReadUsageResponse(conn)
	if err == nil && response.Type == TypeError {
		err = errors.New(response.Msg)
	}
	return response, true, err
}
