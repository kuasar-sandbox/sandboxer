// Package resource defines the reservation protocol used between sandboxer
// and orchestrator. Sandbox-local balloon and cgroup policy is deliberately
// outside this protocol.
package resource

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultSocket is the canonical UDS path the controller listens on.
const DefaultSocket = "/run/sandbox-resource.sock"

// MaxMessageBytes bounds the JSON payload size for one message.
const MaxMessageBytes = 64 * 1024

// DefaultAdminListPageSize is the number of reservations a current client asks
// for per admin_list response. The controller may return fewer entries to keep
// the encoded frame below MaxMessageBytes.
const DefaultAdminListPageSize = 128

// Message types. See docs/node.md §5.2.
const (
	TypeAdmit          = "admit"
	TypeAdmitResponse  = "admit_response"
	TypeSettled        = "settled"
	TypeRequestBudget  = "request_budget"
	TypeBudgetResponse = "budget_response"
	TypeOOMReport      = "oom_report"
	TypeHeartbeat      = "heartbeat"
	TypeRelease        = "release"
	TypeAck            = "ack"
	TypeStateSync      = "state_sync"
	TypeError          = "error"

	TypeAdminDrain  = "admin_drain"
	TypeAdminStatus = "admin_status"
	TypeAdminList   = "admin_list"
)

// Client feature names are carried by Admit and the immutable lifecycle lease.
// Unknown features are ignored so old controllers remain wire-compatible.
const FeatureStateSyncV1 = "state_sync_v1"

// Admission status values.
const (
	StatusAdmitted = "admitted"
	StatusQueued   = "queued"
	StatusRejected = "rejected"
)

// Urgency levels for RequestBudget.
const (
	UrgencyLow    = "low"
	UrgencyNormal = "normal"
	UrgencyHigh   = "high"
)

// Default per-message deadlines.
const (
	DeadlineAdmit         = 30 * time.Second
	DeadlineSettled       = 5 * time.Second
	DeadlineRequestBudget = 5 * time.Second
	DeadlineHeartbeat     = 5 * time.Second
	DeadlineRelease       = 5 * time.Second
)

// Message is the typed envelope. Only fields relevant to Type are
// populated. Length-prefix-JSON wire format matches the launch protocol
// in pkg/proto for consistency.
type Message struct {
	Type  string `json:"type"`
	Token string `json:"token,omitempty"`

	// Admit (sandbox-ctl → controller). Wire names are retained at this
	// reservation boundary: FloorMemoryBytes is settled headroom,
	// StartupBudgetMemory is aligned cold InitialBudget, and
	// AllocatableAtSnapshot is restore BudgetAtSnapshot.
	SandboxID             string   `json:"sandbox_id,omitempty"`
	CapacityMemoryBytes   uint64   `json:"capacity_memory_bytes,omitempty"`
	CapacityCPU           int      `json:"capacity_cpu,omitempty"`
	FloorMemoryBytes      uint64   `json:"floor_memory_bytes,omitempty"`
	FloorCPU              float64  `json:"floor_cpu,omitempty"`
	StartupBudgetMemory   uint64   `json:"startup_budget_memory,omitempty"`
	AllocatableAtSnapshot uint64   `json:"allocatable_at_snapshot,omitempty"`
	CgroupPath            string   `json:"cgroup_path,omitempty"`
	ClientFeatures        []string `json:"client_features,omitempty"`

	// AdmitResponse (controller → sandbox-ctl).
	// Status ∈ {admitted, rejected}. StatusQueued is reserved (server
	// holds the connection on short-term block instead of returning queued).
	Status              string `json:"status,omitempty"`
	GrantedInitialAlloc uint64 `json:"granted_initial_alloc,omitempty"`
	// QueuedETAMs is deprecated (server no longer surfaces Queued status);
	// retained to preserve wire-format BC with older clients.
	QueuedETAMs int64 `json:"queued_eta_ms,omitempty"`
	// QueuedForMs / QueuePosAtIn are informational metadata returned on
	// Admitted: how long the request spent in the server-side FIFO queue
	// before being granted (0 means immediate admit), and the queue depth
	// at insert time. Useful for log/audit; does not affect client logic.
	QueuedForMs  int64 `json:"queued_for_ms,omitempty"`
	QueuePosAtIn int64 `json:"queue_pos_at_in,omitempty"`

	// Settled / Heartbeat. CurrentRSS is host VMM cgroup memory.current
	// diagnostics; it is never guest demand or a Budget input.
	CurrentRSS          uint64 `json:"current_rss,omitempty"`
	CurrentCPUUsec      uint64 `json:"current_cpu_usec,omitempty"`
	RecentHighCount     uint64 `json:"recent_high_count,omitempty"`
	CPUThrottledPeriods uint64 `json:"cpu_throttled_periods,omitempty"`

	// StateSync. AppliedAllocatableMemory is the sandbox's safe reservation
	// baseline. PreviousToken is diagnostic context only; the server
	// authenticates the request from the lease lock and SO_PEERCRED.
	AppliedAllocatableMemory uint64 `json:"applied_allocatable_memory,omitempty"`
	Settled                  bool   `json:"settled,omitempty"`
	PreviousToken            string `json:"previous_token,omitempty"`

	// RequestBudget / BudgetResponse. CurrentAlloc is the sandbox's safe
	// absolute reservation baseline. RequestedDelta=0 commits a shrink.
	CurrentAlloc   uint64 `json:"current_alloc,omitempty"`
	RequestedDelta uint64 `json:"requested_delta,omitempty"`
	Urgency        string `json:"urgency,omitempty"`
	GrantedDelta   uint64 `json:"granted_delta,omitempty"`
	NewAllocatable uint64 `json:"new_allocatable,omitempty"` // resulting/echoed reservation
	CooldownMs     int64  `json:"cooldown_ms,omitempty"`

	// OOMReport.
	OOMCount  uint64 `json:"oom_count,omitempty"`
	KilledPID int    `json:"killed_pid,omitempty"`
	KilledRSS uint64 `json:"killed_rss,omitempty"`

	// AdminDrain.
	Drain bool `json:"drain,omitempty"`

	// AdminStatus response. NodeBudget is the existing name for the configured
	// physical total; HostReserved and OperationalMargin are reported separately.
	// NodeAllocated and Allocated.MemoryBytes are the aggregate live node
	// reservation, not guest demand or host VMM charge.
	Zone              string            `json:"zone,omitempty"`
	NodeAllocated     uint64            `json:"node_allocated_memory,omitempty"`
	AllocatablePool   uint64            `json:"allocatable_pool_memory,omitempty"`
	ReservationCount  int               `json:"reservation_count,omitempty"`
	ProvisionalCount  int               `json:"provisional_count,omitempty"`
	UnknownCount      int               `json:"unknown_count,omitempty"`
	Drained           bool              `json:"drained,omitempty"`
	NodeBudget        ResourcesView     `json:"node_budget,omitempty"`
	HostReserved      ResourcesView     `json:"host_reserved,omitempty"`
	OperationalMargin ResourcesView     `json:"operational_margin,omitempty"`
	Allocated         ResourcesView     `json:"allocated,omitempty"`
	Pool              ResourcesView     `json:"pool,omitempty"`
	StartupInFlight   uint64            `json:"startup_in_flight,omitempty"`
	Reservations      []ReservationView `json:"reservations,omitempty"`

	// AdminList pagination. ListLimit=0 is a legacy unpaginated request;
	// current clients always send a positive limit. ListAfter and ListNext are
	// exclusive lexicographic sandbox-ID cursors. Old peers ignore these fields.
	ListAfter string `json:"list_after,omitempty"`
	ListLimit int    `json:"list_limit,omitempty"`
	ListNext  string `json:"list_next,omitempty"`

	// Generic.
	Reason string `json:"reason,omitempty"`
	Msg    string `json:"msg,omitempty"`
}

// ResourcesView is the protocol-neutral resource pair used by live admin
// status/list replies. CPU is expressed in milli-CPU throughout this protocol.
type ResourcesView struct {
	MemoryBytes uint64 `json:"memory_bytes"`
	CPUMilli    uint64 `json:"cpu_milli"`
}

// ReservationView is a token-free copy of one live controller reservation.
// It is diagnostic output only and is never accepted back as controller state.
// Floor.MemoryBytes is configured guest headroom, AllocatableMemory is the
// current node reservation, EffectiveStartupBytes is the exact initial Budget,
// and CurrentRSS is the host VMM cgroup memory.current diagnostic.
type ReservationView struct {
	SandboxID             string        `json:"sandbox_id"`
	PeerPID               int           `json:"peer_pid,omitempty"`
	CgroupPath            string        `json:"cgroup_path,omitempty"`
	Capacity              ResourcesView `json:"capacity"`
	Floor                 ResourcesView `json:"floor"`
	AllocatableMemory     uint64        `json:"allocatable_memory"`
	EffectiveStartupBytes uint64        `json:"effective_startup_bytes,omitempty"`
	Stage                 string        `json:"stage"`
	CurrentRSS            uint64        `json:"current_rss,omitempty"`
	LastReportUnix        int64         `json:"last_report_unix,omitempty"`
	Connected             bool          `json:"connected"`
	Provisional           bool          `json:"provisional"`
	RecoverySource        string        `json:"recovery_source"`
	StartupExpired        bool          `json:"startup_expired,omitempty"`
}

// WriteMessage writes one message in length-prefix-JSON wire format:
//
//	[4 bytes LE length] [JSON payload]
func WriteMessage(w io.Writer, m *Message) error {
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("nodectl: marshal: %w", err)
	}
	if len(payload) > MaxMessageBytes {
		return fmt.Errorf("nodectl: payload %d > max %d", len(payload), MaxMessageBytes)
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("nodectl: write header: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("nodectl: write payload: %w", err)
	}
	return nil
}

// ReadMessage reads exactly one message.
func ReadMessage(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, errors.New("nodectl: zero-length payload")
	}
	if n > MaxMessageBytes {
		return nil, fmt.Errorf("nodectl: payload %d > max %d", n, MaxMessageBytes)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, fmt.Errorf("nodectl: read payload: %w", err)
	}
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, fmt.Errorf("nodectl: unmarshal: %w", err)
	}
	return &m, nil
}
