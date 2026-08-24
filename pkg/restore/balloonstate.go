package restore

import (
	"encoding/json"
	"fmt"
)

// virtio-balloon PFN shift in CH (matches the linux uapi). Each balloon
// page is 1 << shift = 4096 bytes.
const balloonPFNShift = 12

// chSnapshotTree is the minimal structural shape of cloud-hypervisor's
// state.json that we need to extract the balloon device state. Real
// state.json carries many other branches we ignore.
type chSnapshotTree struct {
	Snapshots    map[string]*chSnapshotTree `json:"snapshots,omitempty"`
	SnapshotData *chSnapshotData            `json:"snapshot_data,omitempty"`
}

type chSnapshotData struct {
	// State is itself a JSON-encoded string per CH's vm-migration crate
	// (each device's state is double-encoded). For balloon, decoding it
	// yields balloonDeviceState below.
	State string `json:"state"`
}

type balloonDeviceState struct {
	Config balloonDeviceConfig `json:"config"`
}

type balloonDeviceConfig struct {
	// NumPages: pages host wants the guest to give up (the target).
	NumPages uint32 `json:"num_pages"`
	// Actual: pages the guest balloon driver has actually given up.
	Actual uint32 `json:"actual"`
}

// parseBalloonFromState walks state.json, finds the balloon device state
// at snapshots["device-manager"].snapshots["__balloon"], and returns its
// target_bytes / current_bytes.
//
// targetBytes  = num_pages * 4096       # what sandbox-ctl told CH via /vm.resize
// currentBytes = actual * 4096          # what guest balloon driver has reported
//
// Returns ok=false (with no error) when no balloon device was captured; that
// means the snapshot Budget is the full CH Capacity. Returns an error only on
// malformed JSON.
func parseBalloonFromState(stateJSON []byte) (targetBytes, currentBytes uint64, ok bool, err error) {
	var tree chSnapshotTree
	if err := json.Unmarshal(stateJSON, &tree); err != nil {
		return 0, 0, false, fmt.Errorf("state.json: %w", err)
	}
	dm, ok2 := tree.Snapshots["device-manager"]
	if !ok2 || dm == nil {
		return 0, 0, false, nil
	}
	bal, ok2 := dm.Snapshots["__balloon"]
	if !ok2 || bal == nil || bal.SnapshotData == nil {
		return 0, 0, false, nil
	}
	var bs balloonDeviceState
	if err := json.Unmarshal([]byte(bal.SnapshotData.State), &bs); err != nil {
		return 0, 0, false, fmt.Errorf("balloon state inner: %w", err)
	}
	targetBytes = uint64(bs.Config.NumPages) << balloonPFNShift
	currentBytes = uint64(bs.Config.Actual) << balloonPFNShift
	return targetBytes, currentBytes, true, nil
}

// deriveBudgetAtSnapshot computes the safe runtime Budget upper bound captured
// by CH.
//
//	BudgetAtSnapshot = Capacity - min(BalloonTarget, BalloonCurrent)
//
// The min() picks whichever balloon interpretation gives the LARGER
// Budget, conservatively preserving guest's effective working set
// even when balloon target/current are still converging at snapshot time:
//   - inflating (current < target): pick current → guest still has the
//     larger memory until driver catches up
//   - deflating (target < current): pick target → host intends to give
//     guest the larger memory; driver hasn't expanded yet
//
// Returns capacity itself when balloon info is unavailable (ok=false),
// matching cold-start behaviour.
func deriveBudgetAtSnapshot(capacityBytes, target, current uint64, ok bool) uint64 {
	if !ok {
		return capacityBytes
	}
	min := target
	if current < min {
		min = current
	}
	if min >= capacityBytes {
		return 0
	}
	return capacityBytes - min
}

// validateBudgetAtSnapshot protects the existing Admit boundary, where zero
// means a cold start rather than a restore. A zero restore Budget also cannot
// satisfy the independently positive settled headroom contract.
func validateBudgetAtSnapshot(capacity, budget uint64) error {
	if capacity == 0 {
		return fmt.Errorf("restore Capacity must be positive")
	}
	if budget == 0 || budget > capacity {
		return fmt.Errorf("restore BudgetAtSnapshot %d is outside (0, %d]", budget, capacity)
	}
	return nil
}

func validateRestoreBalloonControl(capacity, headroom uint64, hasBalloon bool) error {
	if capacity == 0 || headroom == 0 || headroom > capacity {
		return fmt.Errorf("restore memory bounds headroom=%d Capacity=%d", headroom, capacity)
	}
	if !hasBalloon && headroom < capacity {
		return fmt.Errorf("restore enables memory balloon control (headroom=%d Capacity=%d) but snapshot has no balloon device", headroom, capacity)
	}
	return nil
}
