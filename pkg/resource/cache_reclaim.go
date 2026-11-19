package resource

// Both reclaim sources use the same finite, asynchronous cache executor.
const (
	CacheReclaimAutonomous = "autonomous"
	CacheReclaimYield      = "yield"
)

// CacheReclaimIntent is one finite observation, not an accumulating target.
// Repeated reports of the same ID may only reduce its unissued Bytes. A probe
// revalidates a previously fruitless observation and is not autonomous supply
// until the node has issued its finite task.
type CacheReclaimIntent struct {
	ID          uint64 `json:"id"`
	Bytes       uint64 `json:"bytes"`
	Observation uint64 `json:"observation"`
	Probe       bool   `json:"probe,omitempty"`
}

// CacheReclaimTask is immutable authority for at most MaxBytes in one attempt.
// RunID survives reservation-token rotation; Seq is monotonic within that run.
// Target is required: Target+MaxBytes is the confirmed issue-time capacity,
// retained across recovery. The target never enlarges the byte authority.
type CacheReclaimTask struct {
	RunID    string `json:"run_id"`
	Seq      uint64 `json:"seq"`
	Source   string `json:"source"`
	IntentID uint64 `json:"intent_id,omitempty"`
	MaxBytes uint64 `json:"max_bytes"`
	Target   uint64 `json:"target"`
}

// CacheReclaimDone remains in the client outbox until Seq is explicitly ACKed.
// Current is the physically completed absolute capacity at completion time;
// duplicate completions must not reapply this historical baseline after growth.
// A nil Intent explicitly means there is no follow-on autonomous opportunity.
type CacheReclaimDone struct {
	Task    CacheReclaimTask    `json:"task"`
	Current uint64              `json:"current"`
	Intent  *CacheReclaimIntent `json:"intent,omitempty"`
}

// CacheReclaimState is also included in StateSync. LastSeq is retained after
// a completion is acknowledged so a node restart cannot reuse an old sequence.
type CacheReclaimState struct {
	RunID string `json:"run_id"`
	// Current is the worker's last confirmed physical capacity. An accepted
	// growth grant can exceed it until local page allocation finishes. Pure
	// observations never change the node ledger merely by reporting Current.
	Current uint64              `json:"current"`
	LastSeq uint64              `json:"last_seq"`
	Intent  *CacheReclaimIntent `json:"intent,omitempty"`
	Task    *CacheReclaimTask   `json:"task,omitempty"`
	Done    *CacheReclaimDone   `json:"done,omitempty"`
}

// CacheReclaimStatus reports node coordination independently of Guest memory.
type CacheReclaimStatus struct {
	RateBytesPerSecond uint64 `json:"rate_bytes_per_second"`
	BurstBytes         uint64 `json:"burst_bytes"`
	InFlightBytes      uint64 `json:"in_flight_bytes"`
	InFlightTasks      int    `json:"in_flight_tasks"`
	AutonomousIntents  int    `json:"autonomous_intents"`
	HeldIntents        int    `json:"held_intents"`
	RecoveryPending    int    `json:"recovery_pending"`
	LegacyParticipants int    `json:"legacy_participants"`
}

// CloneCacheReclaimState returns an independent snapshot for asynchronous RPCs.
func CloneCacheReclaimState(in *CacheReclaimState) *CacheReclaimState {
	if in == nil {
		return nil
	}
	out := *in
	out.Intent = CloneCacheReclaimIntent(in.Intent)
	if in.Task != nil {
		task := *in.Task
		out.Task = &task
	}
	if in.Done != nil {
		done := *in.Done
		done.Intent = CloneCacheReclaimIntent(in.Done.Intent)
		out.Done = &done
	}
	return &out
}

func CloneCacheReclaimIntent(in *CacheReclaimIntent) *CacheReclaimIntent {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
