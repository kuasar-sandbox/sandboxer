package vhost

import (
	"context"
	"sync"
)

// pauseGate retains the existing snapshot exclusion, while allowing a
// stopped worker to leave the gate before teardown joins it. It does not
// release the inflight identity of a read already in progress.
type pauseGate struct {
	once      sync.Once
	available chan struct{}
}

func (g *pauseGate) init() {
	g.once.Do(func() { g.available = make(chan struct{}, 1); g.available <- struct{}{} })
}

func (g *pauseGate) lock(ctx context.Context) error {
	g.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.available:
		return nil
	}
}

func (g *pauseGate) Lock()   { _ = g.lock(context.Background()) }
func (g *pauseGate) Unlock() { g.available <- struct{}{} }
