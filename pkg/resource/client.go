package resource

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
	conn, err := net.DialTimeout("unix", c.SocketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("client: dial %s: %w", c.SocketPath, err)
	}
	c.conn = conn
	return nil
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

// OwnPreparedReservation makes this client responsible for releasing a token
// that was durably prepared before sandbox-ctl started. Reattach later confirms
// the same reservation; failures before then still retain enough information
// for Release to return the capacity.
func (c *Client) OwnPreparedReservation(token string) error {
	if token == "" {
		return errors.New("client: prepared reservation token is empty")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.token != token {
		return errors.New("client: already owns a different reservation token")
	}
	c.token = token
	return nil
}

// roundTrip writes req and reads exactly one reply. Holds the mutex
// for the duration. Caller must avoid concurrent calls.
func (c *Client) roundTrip(req *Message, deadline time.Duration) (*Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil, errors.New("client: not connected")
	}
	if deadline > 0 {
		_ = c.conn.SetDeadline(time.Now().Add(deadline))
	}
	if err := WriteMessage(c.conn, req); err != nil {
		return nil, err
	}
	resp, err := ReadMessage(c.conn)
	if err != nil {
		return nil, err
	}
	_ = c.conn.SetDeadline(time.Time{})
	if resp.Type == TypeError {
		return resp, fmt.Errorf("controller: %s", resp.Msg)
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
	resp, err := c.roundTrip(&Message{
		Type:                  TypeAdmit,
		SandboxID:             p.SandboxID,
		CapacityMemoryBytes:   p.CapacityMemoryBytes,
		CapacityCPU:           p.CapacityCPU,
		FloorMemoryBytes:      p.FloorMemoryBytes,
		FloorCPU:              p.FloorCPU,
		StartupBudgetMemory:   p.StartupBudgetMemory,
		AllocatableAtSnapshot: p.AllocatableAtSnapshot,
		CgroupPath:            p.CgroupPath,
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

// Reattach binds a fresh connection to an existing reservation and returns the
// reservation's current allocatable-memory grant. Cluster launches use this
// path after node-ctl has already performed durable Admission; they must not
// submit a second Admit request from sandbox-ctl.
func (c *Client) Reattach(token string) (uint64, error) {
	if token == "" {
		return 0, errors.New("client: reattach token is empty")
	}
	c.mu.Lock()
	owned := c.token
	c.mu.Unlock()
	if owned != "" && owned != token {
		return 0, errors.New("client: reattach token does not match the owned reservation")
	}
	resp, err := c.roundTrip(&Message{Type: TypeReattach, Token: token}, DeadlineAdmit)
	if err != nil {
		return 0, err
	}
	if resp.Type != TypeAck {
		return 0, fmt.Errorf("client: reattach reply %q msg=%q", resp.Type, resp.Msg)
	}
	if resp.NewAllocatable == 0 {
		return 0, errors.New("client: reattach acknowledgement omitted a positive allocatable grant")
	}
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
	return resp.NewAllocatable, nil
}

// Settled marks the per-sandbox state machine's startup → settled
// transition. Called by the launch hello handler / SendRestore success
// path.
func (c *Client) Settled(rss, cpuUsec uint64) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return errors.New("client: no token")
	}
	resp, err := c.roundTrip(&Message{
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
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		err = errors.New("client: no token")
		return
	}
	resp, e := c.roundTrip(&Message{
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
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return nil, errors.New("client: no token")
	}
	resp, err := c.roundTrip(&Message{
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
	Zone             string
	NodeAllocated    uint64
	AllocatablePool  uint64
	ReservationCount int
	Drained          bool
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
		Zone:             resp.Zone,
		NodeAllocated:    resp.NodeAllocated,
		AllocatablePool:  resp.AllocatablePool,
		ReservationCount: resp.ReservationCount,
		Drained:          resp.Drained,
	}, nil
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
