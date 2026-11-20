package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

const HostRetirementVersion = 1
const CheckpointLifecycleVersion = 2
const HostRetirementEnvironment = "SANDBOX_CHECKPOINT_RETIREMENT"
const TypeCaptureStateRequest = "capture_state_request"
const TypeCaptureStateResponse = "capture_state_response"
const CaptureStateUnsupportedCode = "capture_state_unsupported"
const MemorySavePendingCode = "memory_save_pending"
const CaptureRejectedCode = "capture_rejected"

var ErrCaptureStateUnsupported = errors.New("capture state unsupported")
var ErrMemorySavePending = errors.New("previous checkpoint memory save is pending")
var ErrCaptureRejected = errors.New("capture rejected before commit")

type HostRetirementPermit struct {
	Version      int    `json:"version"`
	SandboxID    string `json:"sandbox_id"`
	InstanceID   string `json:"instance_id"`
	RunID        string `json:"run_id"`
	CheckpointID string `json:"checkpoint_id"`
	OperationID  string `json:"operation_id"`
}

func (p HostRetirementPermit) Validate() error {
	if p.Version != HostRetirementVersion || p.SandboxID == "" || p.InstanceID == "" || p.RunID == "" || p.CheckpointID == "" || p.OperationID == "" {
		return errors.New("invalid host retirement permit")
	}
	return nil
}

func ParseHostRetirement(raw string) (*HostRetirementPermit, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 4096 {
		return nil, errors.New("oversized host retirement permit")
	}
	var p HostRetirementPermit
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

type CaptureStateQuery struct {
	// ForCapture asks for current-run capture capability. Historical holders
	// never block capture. False reports whether any capture still retains RAM.
	ForCapture                  bool   `json:"for_capture,omitempty"`
	SandboxID                   string `json:"sandbox_id"`
	InstanceID                  string `json:"instance_id"`
	RunID                       string `json:"run_id"`
	NoWaitForPreviousMemorySave bool   `json:"no_wait_for_previous_memory_save,omitempty"`
}

type CaptureState struct {
	LifecycleVersion int `json:"lifecycle_version"`
	// RetentionSupported is populated for capture-admission queries. Nil is
	// an older peer with no capability answer, not evidence of a sync path.
	RetentionSupported *bool  `json:"retention_supported,omitempty"`
	SandboxID          string `json:"sandbox_id"`
	InstanceID         string `json:"instance_id"`
	RunID              string `json:"run_id"`
	MemoryID           string `json:"memory_id,omitempty"`
	Epoch              uint64 `json:"epoch,omitempty"`
	Pending            bool   `json:"pending"`
}

func ReadCaptureState(ctx context.Context, socket string, q CaptureStateQuery) (CaptureState, error) {
	c, close, err := dialContext(ctx, socket)
	if err != nil {
		return CaptureState{}, err
	}
	defer close()
	if err = WriteMessage(c, Request{Type: TypeCaptureStateRequest, CaptureStateQuery: &q}); err != nil {
		return CaptureState{}, err
	}
	var r Response
	if err = ReadMessage(c, &r); err != nil {
		return CaptureState{}, err
	}
	if r.Type == TypeError {
		if r.Code == CaptureStateUnsupportedCode || r.Msg == "unknown type: "+TypeCaptureStateRequest {
			return CaptureState{}, ErrCaptureStateUnsupported
		}
		return CaptureState{}, fmt.Errorf("capture state: %s", r.Msg)
	}
	if r.Type != TypeCaptureStateResponse || r.CaptureState == nil {
		return CaptureState{}, errors.New("invalid capture state response")
	}
	s := *r.CaptureState
	if s.SandboxID != q.SandboxID || s.InstanceID != q.InstanceID || s.RunID != q.RunID {
		return CaptureState{}, errors.New("capture state run identity mismatch")
	}
	return s, nil
}

// Only this read-only request is cancelled on peer disappearance. Accepted
// snapshot requests retain their existing finish-on-disconnect semantics.
func (s *Server) serveCaptureState(c *net.UnixConn, req Request) {
	defer c.Close()
	if s.CaptureStateHandler == nil {
		_ = WriteMessage(c, Response{Type: TypeError, Code: CaptureStateUnsupportedCode, Msg: CaptureStateUnsupportedCode})
		return
	}
	if req.CaptureStateQuery == nil {
		_ = WriteMessage(c, Response{Type: TypeError, Msg: "missing capture state query"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); var one [1]byte; _, _ = c.Read(one[:]); cancel() }()
	defer func() { _ = c.Close(); <-done }()
	state, err := s.CaptureStateHandler(ctx, *req.CaptureStateQuery)
	if err != nil {
		_ = WriteMessage(c, Response{Type: TypeError, Msg: err.Error()})
		return
	}
	_ = WriteMessage(c, Response{Type: TypeCaptureStateResponse, CaptureState: &state})
}

// A waiting snapshot has not entered the capture gate yet. Stop its wait if
// the requester disappears, then detach this cancellation at admission so an
// accepted capture retains its finish-on-disconnect contract.
func (s *Server) admitSnapshot(c *net.UnixConn, req Request) error {
	if s.SnapshotAdmissionHandler == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var admitted atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		var one [1]byte
		_, _ = c.Read(one[:])
		if !admitted.Load() {
			cancel()
		}
	}()
	err := s.SnapshotAdmissionHandler(ctx, req)
	admitted.Store(true)
	_ = c.SetReadDeadline(time.Now())
	<-done
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		return err
	}
	return ctx.Err()
}

func captureErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrCheckpointUnsupported):
		return CheckpointUnsupportedCode
	case errors.Is(err, ErrMemorySavePending):
		return MemorySavePendingCode
	case errors.Is(err, ErrCaptureRejected):
		return CaptureRejectedCode
	}
	return ""
}
