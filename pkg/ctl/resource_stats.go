package ctl

import (
	"context"
	"errors"
)

// ResourceStats is a read of the owner's effective configuration and current
// VMM cgroup. Host counters are independently optional. CPU stays in lossless
// microseconds on the ctl transport; the native HTTP adapter publishes seconds.
// There is no node reservation here: that belongs to the resource controller.
type ResourceStats struct {
	SandboxID      string  `json:"sandbox_id"`
	CPUCapacity    int     `json:"cpu_capacity"`
	CPUAllocatable float64 `json:"cpu_allocatable"`
	MemoryCapacity uint64  `json:"memory_capacity,string"`
	MemoryHeadroom uint64  `json:"memory_headroom,string"`
	MemoryUsed     *uint64 `json:"memory_used,omitempty,string"`
	CPUUsageUsec   *uint64 `json:"cpu_usage_usec,omitempty,string"`
	TimestampUnix  *int64  `json:"timestamp_unix,omitempty"`
}

// ReadResourceStats reads an already-running owner without contacting the
// guest. The caller supplies its authoritative ctl path and exact SandboxID.
func ReadResourceStats(ctx context.Context, socket, sandboxID string) (stats ResourceStats, err error) {
	if sandboxID == "" {
		return ResourceStats{}, errors.New("ctl: resource stats requires sandbox identity")
	}
	conn, close, err := dialStats(ctx, socket)
	if err != nil {
		return ResourceStats{}, err
	}
	defer close()
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	if err := WriteMessage(conn, Request{Type: TypeResourceStatsRequest}); err != nil {
		return ResourceStats{}, err
	}
	var response Response
	if err := ReadMessage(conn, &response); err != nil {
		return ResourceStats{}, err
	}
	if response.Type == TypeError {
		return ResourceStats{}, errors.New(response.Msg)
	}
	if response.Type != TypeResourceStatsResponse || response.ResourceStats == nil || response.ResourceStats.SandboxID != sandboxID {
		return ResourceStats{}, errors.New("ctl: invalid resource stats response or sandbox identity")
	}
	return *response.ResourceStats, nil
}
