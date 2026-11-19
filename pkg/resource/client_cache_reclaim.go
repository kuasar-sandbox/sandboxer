package resource

import (
	"context"
	"errors"
	"fmt"
)

type CacheReclaimResult struct {
	Reservation uint64
	Task        *CacheReclaimTask
	Ack         uint64
}

// UpdateCacheReclaimContext updates background intent or completes one task.
// It never creates, withdraws, or grants a cache-growth request.
func (c *Client) UpdateCacheReclaimContext(ctx context.Context, state CacheReclaimState) (CacheReclaimResult, error) {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token == "" {
		return CacheReclaimResult{}, errors.New("client: no token")
	}
	resp, err := c.roundTripContext(ctx, &Message{
		Type: TypeCacheReclaimUpdate, Token: token,
		CacheReclaim: CloneCacheReclaimState(&state),
	}, DeadlineRequestBudget)
	if err != nil {
		return CacheReclaimResult{}, err
	}
	if resp.Type != TypeAck {
		return CacheReclaimResult{}, fmt.Errorf("client: cache reclaim reply %q", resp.Type)
	}
	return CacheReclaimResult{Reservation: resp.CacheReservation, Task: resp.CacheReclaimTask, Ack: resp.CacheReclaimAck}, nil
}
