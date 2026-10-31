package ctl

import (
	"context"
	"errors"
)

// ReadUsage reports whether the owner was connected so callers cannot silently
// replace an online error with a read of its unaccepted file tail.
func ReadUsage(ctx context.Context, socket string, history bool, cursor int64, limit int) (response Response, connected bool, err error) {
	conn, close, err := dialContext(ctx, socket)
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
