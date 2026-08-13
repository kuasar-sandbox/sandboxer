package resource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// TransportError marks a failed stream operation. Callers use it to enter the
// reconnect path without treating a controller restart as a sandbox failure.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// ControllerError is an explicit TypeError response. It is distinct from a
// broken stream, which matters for StateSync -> legacy Reattach negotiation.
type ControllerError struct{ Message string }

func (e *ControllerError) Error() string { return "controller: " + e.Message }

func IsTransportError(err error) bool {
	var target *TransportError
	return errors.As(err, &target)
}

func IsStateSyncUnsupported(err error) bool {
	var target *ControllerError
	return errors.As(err, &target) && strings.Contains(target.Message, "unknown type: "+TypeStateSync)
}

// Client is sandbox-ctl's RPC interface to the controller. Designed for
// a single long-lived connection per sandbox. Concurrent calls are
// serialized by an internal mutex — sandbox-ctl issues at most one
// outstanding request at a time. Controller-driven allocatable changes
// (active reclaim / admin grant / admin reclaim) are delivered on the
// Heartbeat response (NewAllocatable), not via an unsolicited push.
type Client struct {
	SocketPath string

	mu    sync.Mutex
	conn  net.Conn
	token string
}

// Connect dials the controller socket. Idempotent: re-dialing closes
// the previous connection.
func (c *Client) Connect() error {
	return c.ConnectContext(context.Background())
}

func (c *Client) ConnectContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return fmt.Errorf("client: dial %s: %w", c.SocketPath, err)
	}
	c.conn = conn
	return nil
}

func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Close terminates the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// Token returns the reservation token after a successful Admit /
// Reattach.
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// roundTrip writes req and reads exactly one reply. Holds the mutex
// for the duration. Caller must avoid concurrent calls.
func (c *Client) roundTrip(req *Message, deadline time.Duration) (*Message, error) {
	return c.roundTripContext(context.Background(), req, deadline)
}

func (c *Client) roundTripContext(ctx context.Context, req *Message, deadline time.Duration) (*Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, errors.New("client: not connected")
	}
	conn := c.conn
	deadlineAt := time.Time{}
	if deadline > 0 {
		deadlineAt = time.Now().Add(deadline)
	}
	if ctxDeadline, ok := ctx.Deadline(); ok && (deadlineAt.IsZero() || ctxDeadline.Before(deadlineAt)) {
		deadlineAt = ctxDeadline
	}
	if !deadlineAt.IsZero() {
		_ = conn.SetDeadline(deadlineAt)
	}
	// net.Conn permits concurrent deadline updates. Interrupt an in-flight
	// read/write on cancellation without waiting for Client.mu, which this RPC
	// deliberately holds for the one-outstanding-request contract.
	stopContextWatch := func() {}
	if ctxDone := ctx.Done(); ctxDone != nil {
		stopWatch := make(chan struct{})
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-ctxDone:
				_ = conn.SetDeadline(time.Now())
			case <-stopWatch:
			}
		}()
		var stopOnce sync.Once
		stopContextWatch = func() {
			stopOnce.Do(func() {
				close(stopWatch)
				<-watchDone
			})
		}
	}
	defer stopContextWatch()

	if err := WriteMessage(conn, req); err != nil {
		_ = conn.Close()
		c.conn = nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &TransportError{Err: err}
	}
	resp, err := ReadMessage(conn)
	if err != nil {
		_ = conn.Close()
		c.conn = nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &TransportError{Err: err}
	}
	stopContextWatch()
	_ = conn.SetDeadline(time.Time{})
	if resp.Type == TypeError {
		return resp, &ControllerError{Message: resp.Msg}
	}
	return resp, nil
}

// AdmitParams collects the inputs to an Admit call. Built directly
// from sandbox.SandboxConfig at the lifecycle layer.
type AdmitParams struct {
	SandboxID             string
	CapacityMemoryBytes   uint64
	CapacityCPU           int
	FloorMemoryBytes      uint64
	FloorCPU              float64
	StartupBudgetMemory   uint64
	AllocatableAtSnapshot uint64 // 0 for cold start
	CgroupPath            string
	ClientFeatures        []string
}

// AdmitResult captures the response from an Admit call.
//
// Status is admitted or rejected (StatusQueued is deprecated — the
// controller now holds the connection on short-term block instead of
// surfacing Queued through the API). Reason is the machine-readable
// reject classification when Status==rejected. QueuedForMs / QueuePosAtIn
// are informational metadata returned on admitted: how long the request
// spent in the server-side FIFO queue before being granted, and the
// queue depth at the moment the entry was inserted.
type AdmitResult struct {
	Status              string
	Token               string
	GrantedInitialAlloc uint64
	Reason              string // machine-readable reject code
	Msg                 string // human-readable detail
	QueuedForMs         int64
	QueuePosAtIn        int64

	// QueuedETAMs is retained for wire-format BC; never populated by the
	// modern controller (which never returns StatusQueued).
	QueuedETAMs int64
}

// Admit performs the initial handshake. The sandbox-ctl caller continues
// on Status==StatusAdmitted, aborts on Status==StatusRejected. There is
// no longer a Queued path — the controller blocks the connection while
// the admission worker holds it in the server-side FIFO queue, so the
// Admit call simply takes as long as queuing takes.
func (c *Client) Admit(p AdmitParams) (*AdmitResult, error) {
	return c.AdmitContext(context.Background(), p)
}

func (c *Client) AdmitContext(ctx context.Context, p AdmitParams) (*AdmitResult, error) {
	resp, err := c.roundTripContext(ctx, &Message{
		Type:                  TypeAdmit,
		SandboxID:             p.SandboxID,
		CapacityMemoryBytes:   p.CapacityMemoryBytes,
		CapacityCPU:           p.CapacityCPU,
		FloorMemoryBytes:      p.FloorMemoryBytes,
		FloorCPU:              p.FloorCPU,
		StartupBudgetMemory:   p.StartupBudgetMemory,
		AllocatableAtSnapshot: p.AllocatableAtSnapshot,
		CgroupPath:            p.CgroupPath,
		ClientFeatures:        append([]string(nil), p.ClientFeatures...),
	}, DeadlineAdmit)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAdmitResponse {
		return nil, fmt.Errorf("client: unexpected admit reply %q", resp.Type)
	}
	c.mu.Lock()
	c.token = resp.Token
	c.mu.Unlock()
	return &AdmitResult{
		Status:              resp.Status,
		Token:               resp.Token,
		GrantedInitialAlloc: resp.GrantedInitialAlloc,
		Reason:              resp.Reason,
		Msg:                 resp.Msg,
		QueuedForMs:         resp.QueuedForMs,
		QueuePosAtIn:        resp.QueuePosAtIn,
		QueuedETAMs:         resp.QueuedETAMs,
	}, nil
}

// Reattach binds a fresh connection to an existing reservation
// (controller restart or transient network drop).
func (c *Client) Reattach(token string) error {
	_, err := c.ReattachState(token)
	return err
}

// ReattachState is the compatibility path for controllers that predate
// StateSync. It also returns the old controller's current allocation so the
// caller can apply it before resuming requests.
func (c *Client) ReattachState(token string) (uint64, error) {
	return c.ReattachStateContext(context.Background(), token)
}

func (c *Client) ReattachStateContext(ctx context.Context, token string) (uint64, error) {
	resp, err := c.roundTripContext(ctx, &Message{Type: TypeReattach, Token: token}, DeadlineAdmit)
	if err != nil {
		return 0, err
	}
	if resp.Type != TypeAck {
		return 0, fmt.Errorf("client: reattach reply %q msg=%q", resp.Type, resp.Msg)
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	return resp.NewAllocatable, nil
}

type StateSyncParams struct {
	SandboxID                string
	AppliedAllocatableMemory uint64
	Settled                  bool
	CurrentRSS               uint64
	PreviousToken            string
}

type StateSyncResult struct {
	Token          string
	NewAllocatable uint64
}

func (c *Client) StateSync(p StateSyncParams) (*StateSyncResult, error) {
	return c.StateSyncContext(context.Background(), p)
}

func (c *Client) StateSyncContext(ctx context.Context, p StateSyncParams) (*StateSyncResult, error) {
	resp, err := c.roundTripContext(ctx, &Message{
		Type:                     TypeStateSync,
		SandboxID:                p.SandboxID,
		AppliedAllocatableMemory: p.AppliedAllocatableMemory,
		Settled:                  p.Settled,
		CurrentRSS:               p.CurrentRSS,
		PreviousToken:            p.PreviousToken,
	}, DeadlineAdmit)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAck || resp.Token == "" {
		return nil, fmt.Errorf("client: state_sync reply %q msg=%q", resp.Type, resp.Msg)
	}
	c.mu.Lock()
	c.token = resp.Token
	c.mu.Unlock()
	return &StateSyncResult{Token: resp.Token, NewAllocatable: resp.NewAllocatable}, nil
}

// Settled marks the per-sandbox state machine's startup → settled
// transition. Called by the launch hello handler / SendRestore success
// path.
func (c *Client) Settled(rss, cpuUsec uint64) error {
	return c.SettledContext(context.Background(), rss, cpuUsec)
}

func (c *Client) SettledContext(ctx context.Context, rss, cpuUsec uint64) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return errors.New("client: no token")
	}
	resp, err := c.roundTripContext(ctx, &Message{
		Type:           TypeSettled,
		Token:          tok,
		CurrentRSS:     rss,
		CurrentCPUUsec: cpuUsec,
	}, DeadlineSettled)
	if err != nil {
		return err
	}
	if resp.Type != TypeAck {
		return fmt.Errorf("client: settled reply %q", resp.Type)
	}
	return nil
}

// RequestBudget asks for a memory budget delta. Returns granted_delta
// (may be 0) and the cooldown the caller should respect before retrying.
func (c *Client) RequestBudget(currentAlloc, requestedDelta uint64, urgency, reason string) (granted uint64, newAlloc uint64, cooldownMs int64, err error) {
	return c.RequestBudgetContext(context.Background(), currentAlloc, requestedDelta, urgency, reason)
}

func (c *Client) RequestBudgetContext(ctx context.Context, currentAlloc, requestedDelta uint64, urgency, reason string) (granted uint64, newAlloc uint64, cooldownMs int64, err error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		err = errors.New("client: no token")
		return
	}
	resp, e := c.roundTripContext(ctx, &Message{
		Type:           TypeRequestBudget,
		Token:          tok,
		CurrentAlloc:   currentAlloc,
		RequestedDelta: requestedDelta,
		Urgency:        urgency,
		Reason:         reason,
	}, DeadlineRequestBudget)
	if e != nil {
		err = e
		return
	}
	if resp.Type != TypeBudgetResponse {
		err = fmt.Errorf("client: budget reply %q", resp.Type)
		return
	}
	return resp.GrantedDelta, resp.NewAllocatable, resp.CooldownMs, nil
}

// HeartbeatResult holds the controller's view of the sandbox state at
// heartbeat time. NewAllocatable is the authoritative allocatable_now —
// when it differs from the local value, the active reclaimer or an
// admin command has changed it and the caller must apply (cgroup
// memory.high + balloon resize).
type HeartbeatResult struct {
	NewAllocatable uint64
}

// Heartbeat sends a periodic alive notification with current usage and
// returns the controller's authoritative allocatable_now for delta-
// tracking.
func (c *Client) Heartbeat(rss, cpuUsec, recentHigh, cpuThrottled uint64) (*HeartbeatResult, error) {
	return c.HeartbeatContext(context.Background(), rss, cpuUsec, recentHigh, cpuThrottled)
}

func (c *Client) HeartbeatContext(ctx context.Context, rss, cpuUsec, recentHigh, cpuThrottled uint64) (*HeartbeatResult, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return nil, errors.New("client: no token")
	}
	resp, err := c.roundTripContext(ctx, &Message{
		Type:                TypeHeartbeat,
		Token:               tok,
		CurrentRSS:          rss,
		CurrentCPUUsec:      cpuUsec,
		RecentHighCount:     recentHigh,
		CPUThrottledPeriods: cpuThrottled,
	}, DeadlineHeartbeat)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAck {
		return nil, fmt.Errorf("client: heartbeat reply %q", resp.Type)
	}
	return &HeartbeatResult{NewAllocatable: resp.NewAllocatable}, nil
}

// AdminDrain toggles drain mode on the controller.
func (c *Client) AdminDrain(enable bool) error {
	resp, err := c.roundTrip(&Message{Type: TypeAdminDrain, Drain: enable}, DeadlineHeartbeat)
	if err != nil {
		return err
	}
	if resp.Type != TypeAck {
		return fmt.Errorf("client: admin_drain reply %q msg=%q", resp.Type, resp.Msg)
	}
	return nil
}

// AdminGrant force-grants delta bytes of memory to a sandbox identified
// by sid. Returns the controller's new allocatable_now value.
func (c *Client) AdminGrant(sid string, delta uint64) (uint64, error) {
	resp, err := c.roundTrip(&Message{
		Type:           TypeAdminGrant,
		SandboxID:      sid,
		RequestedDelta: delta,
	}, DeadlineRequestBudget)
	if err != nil {
		return 0, err
	}
	if resp.Type != TypeAck {
		return 0, fmt.Errorf("client: admin_grant reply %q msg=%q", resp.Type, resp.Msg)
	}
	return resp.NewAllocatable, nil
}

// AdminReclaim forces a sandbox identified by sid to shrink to target
// allocatable bytes. Returns the new allocatable.
func (c *Client) AdminReclaim(sid string, target uint64) (uint64, error) {
	resp, err := c.roundTrip(&Message{
		Type:              TypeAdminReclaim,
		SandboxID:         sid,
		TargetAllocatable: target,
	}, DeadlineRequestBudget)
	if err != nil {
		return 0, err
	}
	if resp.Type != TypeAck {
		return 0, fmt.Errorf("client: admin_reclaim reply %q msg=%q", resp.Type, resp.Msg)
	}
	return resp.NewAllocatable, nil
}

// AdminStatusResult is the response to AdminStatus.
type AdminStatusResult struct {
	Zone              string
	NodeAllocated     uint64
	AllocatablePool   uint64
	ReservationCount  int
	ProvisionalCount  int
	UnknownCount      int
	Drained           bool
	NodeBudget        ResourcesView
	HostReserved      ResourcesView
	OperationalMargin ResourcesView
	Allocated         ResourcesView
	Pool              ResourcesView
	StartupInFlight   uint64
}

// AdminStatus queries the controller's live state via RPC. Useful when
// state.json is not directly accessible (e.g. cross-host inspection).
func (c *Client) AdminStatus() (*AdminStatusResult, error) {
	resp, err := c.roundTrip(&Message{Type: TypeAdminStatus}, DeadlineHeartbeat)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAck {
		return nil, fmt.Errorf("client: admin_status reply %q msg=%q", resp.Type, resp.Msg)
	}
	return &AdminStatusResult{
		Zone:              resp.Zone,
		NodeAllocated:     resp.NodeAllocated,
		AllocatablePool:   resp.AllocatablePool,
		ReservationCount:  resp.ReservationCount,
		ProvisionalCount:  resp.ProvisionalCount,
		UnknownCount:      resp.UnknownCount,
		Drained:           resp.Drained,
		NodeBudget:        resp.NodeBudget,
		HostReserved:      resp.HostReserved,
		OperationalMargin: resp.OperationalMargin,
		Allocated:         resp.Allocated,
		Pool:              resp.Pool,
		StartupInFlight:   resp.StartupInFlight,
	}, nil
}

func (c *Client) AdminList() ([]ReservationView, error) {
	resp, err := c.roundTrip(&Message{Type: TypeAdminList}, DeadlineHeartbeat)
	if err != nil {
		return nil, err
	}
	if resp.Type != TypeAck {
		return nil, fmt.Errorf("client: admin_list reply %q msg=%q", resp.Type, resp.Msg)
	}
	return append([]ReservationView(nil), resp.Reservations...), nil
}

// OOMReport notifies the controller of a guest-side OOM event.
func (c *Client) OOMReport(oomCount uint64, killedPID int, killedRSS uint64) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return errors.New("client: no token")
	}
	resp, err := c.roundTrip(&Message{
		Type:      TypeOOMReport,
		Token:     tok,
		OOMCount:  oomCount,
		KilledPID: killedPID,
		KilledRSS: killedRSS,
	}, DeadlineHeartbeat)
	if err != nil {
		return err
	}
	if resp.Type != TypeAck {
		return fmt.Errorf("client: oom reply %q", resp.Type)
	}
	return nil
}

// Release notifies the controller of imminent sandbox exit and frees the
// reservation. Best-effort: errors are not fatal because the controller
// can also detect connection drop.
func (c *Client) Release(reason string) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return nil
	}
	resp, err := c.roundTrip(&Message{
		Type:   TypeRelease,
		Token:  tok,
		Reason: reason,
	}, DeadlineRelease)
	if err != nil {
		return err
	}
	if resp.Type != TypeAck {
		return fmt.Errorf("client: release reply %q", resp.Type)
	}
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	return nil
}
