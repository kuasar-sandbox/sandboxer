package resource

import (
	"context"
	"fmt"
	"path/filepath"
)

const FeatureRetainedMemoryV1 = "checkpoint_memory_handoff_v1"

// Only a positive, identity-checked RAMReleased proof permits this fallback.
// It covers a previously static capture that never had a node retention bill.
const ReasonRetentionSettledMissing = "checkpoint_memory_released_without_receipt"

const (
	TypePrepareRetained = "prepare_retained_memory"
	TypeCommitRetained  = "commit_retained_memory"
	TypeAbortRetained   = "abort_retained_memory"
	TypeAdoptRetained   = "adopt_retained_memory"
	TypeReleaseRetained = "release_retained_memory"
)

// CaptureRetention binds an accounting operation to one immutable capture
// identity. CapturePath names its mutable, atomically updated lifecycle record;
// that record remains the sole authority for actual RAM release.
type CaptureRetention struct {
	CapturePath string `json:"capture_path"`
	MemoryID    string `json:"memory_id"`
	OperationID string `json:"operation_id"`
	Epoch       uint64 `json:"epoch"`
}

func (r CaptureRetention) Validate() error {
	if r.MemoryID == "" || r.OperationID == "" || r.Epoch == 0 ||
		!filepath.IsAbs(r.CapturePath) || filepath.Clean(r.CapturePath) != r.CapturePath {
		return fmt.Errorf("invalid checkpoint memory retention identity")
	}
	return nil
}

// RetainedMemory is the one previous frozen RAM object charged to a SID. Its
// source pool never changes when the active runtime moves to a different pool.
type RetainedMemory struct {
	MemoryID   string `json:"memory_id"`
	Epoch      uint64 `json:"epoch"`
	SourcePool string `json:"source_pool"`
	Bytes      uint64 `json:"bytes"`
}

func (c *Client) RetainedMemoryContext(ctx context.Context, typ, sid string, capture CaptureRetention) (*RetainedMemory, error) {
	if err := capture.Validate(); err != nil {
		return nil, err
	}
	switch typ {
	case TypePrepareRetained, TypeCommitRetained, TypeAbortRetained, TypeReleaseRetained:
	default:
		return nil, fmt.Errorf("invalid retention operation %q", typ)
	}
	resp, err := c.roundTripContext(ctx, &Message{Type: typ, SandboxID: sid, Token: c.Token(), Retention: &capture}, DeadlineRelease)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAck {
		return nil, fmt.Errorf("client: unexpected retention reply %q", resp.Type)
	}
	return resp.Retained, nil
}

// AdoptRetainedContext admits only a new active Budget. The previous frozen
// charge stays in its source pool and the candidate lease is published by the
// controller before this call can succeed. Replays retain the same handoff ID.
func (c *Client) AdoptRetainedContext(ctx context.Context, p AdmitParams, capture CaptureRetention, candidate, handoffID string) (*AdmitResult, error) {
	if err := capture.Validate(); err != nil {
		return nil, err
	}
	resp, err := c.roundTripContext(ctx, &Message{
		Type: TypeAdoptRetained, SandboxID: p.SandboxID,
		CapacityMemoryBytes: p.CapacityMemoryBytes, CapacityCPU: p.CapacityCPU,
		FloorMemoryBytes: p.FloorMemoryBytes, FloorCPU: p.FloorCPU,
		StartupBudgetMemory: p.StartupBudgetMemory, AllocatableAtSnapshot: p.AllocatableAtSnapshot,
		CgroupPath: p.CgroupPath, ClientFeatures: append([]string(nil), p.ClientFeatures...),
		CacheMaximum: p.CacheMaximum, CacheInitial: p.CacheInitial, CacheReclaim: CloneCacheReclaimState(p.CacheReclaim),
		Retention: &capture, LeaseCandidate: candidate, HandoffID: handoffID,
	}, DeadlineAdmit)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAdmitResponse {
		return nil, fmt.Errorf("client: unexpected retained admission reply %q", resp.Type)
	}
	if resp.Status == StatusAdmitted && !HasFeature(resp.ClientFeatures, FeatureRetainedMemoryV1) {
		return nil, fmt.Errorf("controller did not confirm checkpoint memory handoff")
	}
	c.mu.Lock()
	c.token = resp.Token
	c.mu.Unlock()
	return &AdmitResult{Status: resp.Status, Token: resp.Token, GrantedInitialAlloc: resp.GrantedInitialAlloc,
		Reason: resp.Reason, Msg: resp.Msg, CacheSupported: HasFeature(resp.ClientFeatures, FeatureCacheBudgetV1),
		CacheCoordinated: HasFeature(resp.ClientFeatures, FeatureCacheReclaimV1), ClientFeatures: append([]string(nil), resp.ClientFeatures...),
		GrantedInitialCache: resp.GrantedInitialCache}, nil
}
