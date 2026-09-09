package proto

const (
	TypeUsageRequest    = "usage_request"
	TypeUsageResponse   = "usage_response"
	MaxUsageFilesystems = 9 // root + config.MaxDataDisks
	UsageOK             = "ok"
	UsageBusy           = "busy"
	UsageTimeout        = "timeout"
	UsageUnsupported    = "unsupported"
	UsageInvalid        = "invalid"
	UsageError          = "error"
)

// UsageRequest starts a new observation; it is never an export/flush request.
// ReadBudgetNS is the Guest's remaining sub-budget, strictly shorter than the
// Host round. RunEpoch/RequestID survive connection replacement.
type UsageRequest struct {
	RunEpoch     string `json:"run_epoch"`
	RequestID    uint64 `json:"request_id,string"`
	ReadBudgetNS int64  `json:"read_budget_ns,string"`
}

// UsageReadState describes one newly executed raw read, or its absence. It has
// no wall-clock timestamp; the Host supplies the actual request window.
type UsageReadState struct {
	Status     string `json:"status"`
	DurationNS int64  `json:"read_duration_ns,string"`
}

type UsageMemory struct {
	UsageReadState
	Domain         string  `json:"domain,omitempty"`
	PresentPages   *uint64 `json:"present_pages,omitempty,string"`
	BuddyFreePages *uint64 `json:"buddy_free_pages,omitempty,string"`
	PCPFreePages   *uint64 `json:"pcp_free_pages,omitempty,string"`
	PageSize       *uint64 `json:"page_size,omitempty,string"`
}

type UsageFilesystem struct {
	UsageReadState
	Disk         string  `json:"disk"`
	Incarnation  string  `json:"incarnation"`
	Blocks       *uint64 `json:"blocks,omitempty,string"`
	BFree        *uint64 `json:"bfree,omitempty,string"`
	BlockSize    *uint64 `json:"block_size,omitempty,string"`
	FragmentSize *uint64 `json:"fragment_size,omitempty,string"`
	Type         *uint64 `json:"fs_type,omitempty,string"`
}

type UsageResponse struct {
	RunEpoch    string            `json:"run_epoch"`
	RequestID   uint64            `json:"request_id,string"`
	Memory      UsageMemory       `json:"memory"`
	Filesystems []UsageFilesystem `json:"filesystems"`
}
