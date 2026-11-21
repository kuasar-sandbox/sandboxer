package resource

import (
	"context"
	"fmt"
	"path/filepath"
)

const FeatureRetainedMemoryV1 = "checkpoint_memory_handoff_v1"

// V2 transfers a node-owned account, never a caller-selected subset of R.
const FeatureRetainedMemoryV2 = "checkpoint_memory_handoff_v2"
const TypeGetRetainedAccount = "get_retained_account_v2"

type RetainedAccount struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

func (r RetainedAccount) Validate() error {
	if r.ID == "" || r.Revision == 0 {
		return fmt.Errorf("invalid retained account reference")
	}
	return nil
}

// RetainedAccountContext obtains a bounded reference to the authoritative
// node inventory, including an empty account with a pending owner handoff.
func (c *Client) RetainedAccountContext(ctx context.Context, sid string) (*RetainedAccount, error) {
	resp, err := c.roundTripContext(ctx, &Message{Type: TypeGetRetainedAccount, SandboxID: sid,
		ClientFeatures: []string{FeatureRetainedMemoryV2}}, DeadlineAdmit)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAck || !HasFeature(resp.ClientFeatures, FeatureRetainedMemoryV2) {
		return nil, fmt.Errorf("controller does not support retained accounts")
	}
	if resp.RetainedAccount != nil {
		if err := resp.RetainedAccount.Validate(); err != nil {
			return nil, err
		}
	}
	return resp.RetainedAccount, nil
}

// Only a positive, identity-checked RAMReleased proof permits this fallback.
// It covers a previously static capture that never had a node retention bill.
const ReasonRetentionAccountChanged = "checkpoint_account_changed"

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

// RetainedMemory is one frozen RAM object charged to a SID. Its
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

// AdoptRetainedContext keeps the existing capture hint API for callers that
// need static-to-dynamic release verification. The node account, not this hint,
// determines every inherited obligation. The wire never contains a capture set.
func (c *Client) AdoptRetainedContext(ctx context.Context, p AdmitParams, capture CaptureRetention, candidate, handoffID string) (*AdmitResult, error) {
	if err := capture.Validate(); err != nil {
		return nil, err
	}
	account, err := c.RetainedAccountContext(ctx, p.SandboxID)
	if err != nil {
		return nil, err
	}
	return c.AdoptRetainedAccountContext(ctx, p, account, &capture, candidate, handoffID)
}

// AdoptRetainedAccountContext freezes the candidate contract at the first
// accepted handoff. Replays may use that reference after individual R settle.
func (c *Client) AdoptRetainedAccountContext(ctx context.Context, p AdmitParams, account *RetainedAccount, fallback *CaptureRetention, candidate, handoffID string) (*AdmitResult, error) {
	if account != nil {
		if err := account.Validate(); err != nil {
			return nil, err
		}
	}
	if !HasFeature(p.ClientFeatures, FeatureRetainedMemoryV2) {
		return nil, fmt.Errorf("retained account capability is required")
	}
	resp, err := c.roundTripContext(ctx, &Message{
		Type: TypeAdoptRetained, SandboxID: p.SandboxID,
		CapacityMemoryBytes: p.CapacityMemoryBytes, CapacityCPU: p.CapacityCPU,
		FloorMemoryBytes: p.FloorMemoryBytes, FloorCPU: p.FloorCPU,
		StartupBudgetMemory: p.StartupBudgetMemory, AllocatableAtSnapshot: p.AllocatableAtSnapshot,
		CgroupPath: p.CgroupPath, ClientFeatures: append([]string(nil), p.ClientFeatures...),
		CacheMaximum: p.CacheMaximum, CacheInitial: p.CacheInitial, CacheReclaim: CloneCacheReclaimState(p.CacheReclaim),
		RetainedAccount: account, Retention: fallback, LeaseCandidate: candidate, HandoffID: handoffID,
	}, DeadlineAdmit)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAdmitResponse {
		return nil, fmt.Errorf("client: unexpected retained admission reply %q", resp.Type)
	}
	if resp.Status == StatusAdmitted && !HasFeature(resp.ClientFeatures, FeatureRetainedMemoryV2) {
		return nil, fmt.Errorf("controller did not confirm retained account handoff")
	}
	c.mu.Lock()
	c.token = resp.Token
	c.mu.Unlock()
	return &AdmitResult{Status: resp.Status, Token: resp.Token, GrantedInitialAlloc: resp.GrantedInitialAlloc,
		Reason: resp.Reason, Msg: resp.Msg, CacheSupported: HasFeature(resp.ClientFeatures, FeatureCacheBudgetV1),
		CacheCoordinated: HasFeature(resp.ClientFeatures, FeatureCacheReclaimV1), ClientFeatures: append([]string(nil), resp.ClientFeatures...),
		GrantedInitialCache: resp.GrantedInitialCache}, nil
}
