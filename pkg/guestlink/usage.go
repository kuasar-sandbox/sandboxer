package guestlink

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// UsageClient owns the dedicated reusable management connection. Its request
// slot belongs to the running sandbox, not a connection, and is never queued.
type UsageClient struct {
	Host      *HostClient
	requestMu sync.Mutex
	mu        sync.Mutex
	conn      net.Conn
}

type UsageWindow struct {
	Started, Finished time.Time
	Response          proto.UsageResponse
}

func (c *UsageClient) Close() {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *UsageClient) Read(ctx context.Context, epoch string, id uint64) (UsageWindow, error) {
	if !c.requestMu.TryLock() {
		return UsageWindow{}, errors.New("usage request busy")
	}
	defer c.requestMu.Unlock()
	start := time.Now()
	end, ok := ctx.Deadline()
	if !ok || !end.After(start) {
		return UsageWindow{}, context.DeadlineExceeded
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		var err error
		conn, err = c.Host.DialRawContext(ctx, time.Until(end))
		if err != nil {
			return UsageWindow{}, err
		}
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
	}
	valid := false
	defer func() {
		if !valid {
			c.mu.Lock()
			if c.conn == conn {
				c.conn = nil
			}
			c.mu.Unlock()
			_ = conn.Close()
		}
	}()
	if err := conn.SetDeadline(end); err != nil {
		return UsageWindow{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	budget := time.Until(end) / 2
	if budget <= 0 {
		return UsageWindow{}, context.DeadlineExceeded
	}
	if budget > time.Second {
		budget = time.Second
	}
	req := proto.UsageRequest{RunEpoch: epoch, RequestID: id, ReadBudgetNS: int64(budget)}
	if err := proto.WriteMessage(conn, &proto.Message{Type: proto.TypeUsageRequest, UsageRequest: &req}); err != nil {
		return UsageWindow{}, err
	}
	msg, err := proto.ReadMessage(conn)
	finish := time.Now()
	if err != nil {
		return UsageWindow{}, err
	}
	if ctx.Err() != nil || finish.After(end) {
		return UsageWindow{}, context.DeadlineExceeded
	}
	if msg.Type != proto.TypeUsageResponse || msg.UsageResponse == nil {
		return UsageWindow{}, errors.New("usage: unsupported response")
	}
	r := msg.UsageResponse
	if r.RunEpoch != epoch || r.RequestID != id || len(r.Filesystems) > proto.MaxUsageFilesystems {
		return UsageWindow{}, errors.New("usage: stale/invalid response identity")
	}
	check := func(state proto.UsageReadState) bool {
		return state.DurationNS >= 0 && state.DurationNS <= int64(budget)
	}
	if !check(r.Memory.UsageReadState) {
		r.Memory.Status = proto.UsageInvalid
	}
	seen := map[string]bool{}
	for i := range r.Filesystems {
		fs := &r.Filesystems[i]
		knownDisk := fs.Disk == "root" || (len(fs.Disk) == 6 && fs.Disk[:5] == "disk-" && fs.Disk[5] >= '0' && fs.Disk[5] <= '7')
		if !knownDisk || len(fs.Incarnation) > 256 || seen[fs.Disk] {
			return UsageWindow{}, errors.New("usage: invalid filesystem identities")
		}
		seen[fs.Disk] = true
		if !check(fs.UsageReadState) {
			fs.Status = proto.UsageInvalid
		}
	}
	if !stop() {
		return UsageWindow{}, context.Canceled
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return UsageWindow{}, err
	}
	valid = true
	return UsageWindow{Started: start, Finished: finish, Response: *r}, nil
}
