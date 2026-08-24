package resctl

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

const balloonPageSize uint64 = 1 << 12

var ErrMemoryOverflow = errors.New("memory arithmetic overflow")

// MemoryBudget is one local calculation from a host-owned Capacity, one CH
// observation and one fresh guest MemAvailable report.
type MemoryBudget struct {
	Capacity          uint64
	Headroom          uint64
	BalloonTarget     uint64
	GuestMemAvailable uint64

	TargetBudget    uint64
	CurrentBudget   uint64
	ObservedBudget  uint64
	DemandMemory    uint64
	RawRequested    uint64
	DesiredTarget   uint64
	RequestedBudget uint64
}

// MemoryHighResult records the integer terms used to derive memory.high.
type MemoryHighResult struct {
	MemoryMax       uint64
	GrantedHeadroom uint64
	PressureReserve uint64
	HostMemoryHigh  uint64
}

func saturatingAdd(a, b uint64) (uint64, bool) {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64, true
	}
	return sum, false
}

func saturatingSub(a, b uint64) (uint64, bool) {
	diff, borrow := bits.Sub64(a, b, 0)
	if borrow != 0 {
		return 0, true
	}
	return diff, false
}

func clampMemory(value, capacity uint64) uint64 {
	if value > capacity {
		return capacity
	}
	return value
}

func alignDown(value, step uint64) uint64 {
	if step == 0 {
		return 0
	}
	return value - value%step
}

// BudgetFromTarget converts a balloon target to its guest Budget.
func BudgetFromTarget(capacity, target uint64) uint64 {
	budget, _ := saturatingSub(capacity, clampMemory(target, capacity))
	return budget
}

// TargetForBudget aligns balloon target down, which can only retain more guest
// Budget and therefore cannot reduce the requested headroom.
func TargetForBudget(capacity, budget uint64) uint64 {
	budget = clampMemory(budget, capacity)
	rawTarget, _ := saturatingSub(capacity, budget)
	return alignDown(rawTarget, resource.MemoryStep)
}

// AlignedBudget returns the Budget represented by TargetForBudget.
func AlignedBudget(capacity, budget uint64) uint64 {
	return BudgetFromTarget(capacity, TargetForBudget(capacity, budget))
}

// alignedBudgetAtMostReservation returns the largest MemoryStep-representable
// Budget that does not exceed an already granted reservation. It is the grow
// counterpart to TargetForBudget: a partial node grant must be accumulated,
// not rounded up into guest memory that has not been reserved yet.
func alignedBudgetAtMostReservation(capacity, reservation uint64) uint64 {
	reservation = clampMemory(reservation, capacity)
	if reservation == capacity {
		return capacity
	}
	rawTarget, _ := saturatingSub(capacity, reservation)
	target := rawTarget
	if remainder := target % resource.MemoryStep; remainder != 0 {
		var overflow bool
		target, overflow = saturatingAdd(target, resource.MemoryStep-remainder)
		if overflow || target > capacity {
			return 0
		}
	}
	return BudgetFromTarget(capacity, target)
}

// CalculateMemoryBudget implements the local #119 formula. currentBudget is
// CH vm.info.memory_actual_size. Guest MemTotal is deliberately not an input.
func CalculateMemoryBudget(capacity, headroom, balloonTarget, currentBudget, guestMemAvailable uint64) (MemoryBudget, error) {
	if capacity == 0 {
		return MemoryBudget{}, errors.New("memory capacity must be positive")
	}
	if headroom == 0 || headroom > capacity {
		return MemoryBudget{}, fmt.Errorf("memory headroom %d is outside (0, %d]", headroom, capacity)
	}
	if balloonTarget > capacity {
		return MemoryBudget{}, fmt.Errorf("balloon target %d exceeds capacity %d", balloonTarget, capacity)
	}
	if currentBudget > capacity {
		return MemoryBudget{}, fmt.Errorf("current Budget %d exceeds capacity %d", currentBudget, capacity)
	}

	targetBudget := BudgetFromTarget(capacity, balloonTarget)
	demand, _ := saturatingSub(currentBudget, guestMemAvailable)
	rawRequested, overflow := saturatingAdd(demand, headroom)
	rawRequested = clampMemory(rawRequested, capacity)
	desiredTarget := TargetForBudget(capacity, rawRequested)
	requestedBudget := BudgetFromTarget(capacity, desiredTarget)
	observed := targetBudget
	if currentBudget > observed {
		observed = currentBudget
	}
	result := MemoryBudget{
		Capacity: capacity, Headroom: headroom, BalloonTarget: balloonTarget,
		GuestMemAvailable: guestMemAvailable, TargetBudget: targetBudget,
		CurrentBudget: currentBudget, ObservedBudget: observed,
		DemandMemory: demand, RawRequested: rawRequested,
		DesiredTarget: desiredTarget, RequestedBudget: requestedBudget,
	}
	if overflow {
		return result, ErrMemoryOverflow
	}
	return result, nil
}

// NextShrinkTarget returns at most one aligned target step. Shrink is allowed
// only from a stable CH observation and only when more than one full step
// remains between the accepted and desired targets. The retained step is the
// shrink deadband; it is Budget headroom, not part of RequestedBudget.
func NextShrinkTarget(capacity, acceptedTarget, currentBudget, requestedBudget uint64) (uint64, bool) {
	if acceptedTarget > capacity || currentBudget > capacity {
		return acceptedTarget, false
	}
	if BudgetFromTarget(capacity, acceptedTarget) != currentBudget {
		return acceptedTarget, false
	}
	desiredTarget := TargetForBudget(capacity, requestedBudget)
	if desiredTarget <= acceptedTarget {
		return acceptedTarget, false
	}
	gap, _ := saturatingSub(desiredTarget, acceptedTarget)
	if gap <= resource.MemoryStep {
		return acceptedTarget, false
	}
	next, overflow := saturatingAdd(acceptedTarget, resource.MemoryStep)
	if overflow || next > desiredTarget {
		next = desiredTarget
	}
	next = alignDown(next, resource.MemoryStep)
	if next <= acceptedTarget {
		return acceptedTarget, false
	}
	return next, true
}

func mulDivCeil(a, b, divisor uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	quotient, remainder := bits.Div64(hi, lo, divisor)
	if remainder != 0 {
		quotient++
	}
	return quotient
}

// CalculateMemoryHigh implements the host VMM cgroup policy. demand is guest
// demand; hostMemoryCurrent is only a safety lower bound against immediate
// self-throttling and never feeds the Budget calculation.
func CalculateMemoryHigh(capacity, overhead, budget, demand, ratio, hostMemoryCurrent uint64) (MemoryHighResult, error) {
	if capacity == 0 {
		return MemoryHighResult{}, errors.New("memory capacity must be positive")
	}
	// The external config is strictly 0 < ratio < 1. Its conservative
	// fixed-point conversion may produce zero for a positive value smaller
	// than one RatioScale unit, so the integer formula accepts [0, scale).
	if ratio >= resource.RatioScale {
		return MemoryHighResult{}, fmt.Errorf("quantized memory.high ratio %d is outside [0, %d)", ratio, resource.RatioScale)
	}
	budget = clampMemory(budget, capacity)
	demand = clampMemory(demand, budget)
	grantedHeadroom, _ := saturatingSub(budget, demand)
	fraction := mulDivCeil(grantedHeadroom, resource.RatioScale-ratio, resource.RatioScale)
	minimum := grantedHeadroom
	if minimum > resource.MemoryStep {
		minimum = resource.MemoryStep
	}
	pressureReserve := fraction
	if pressureReserve < minimum {
		pressureReserve = minimum
	}
	if pressureReserve > grantedHeadroom {
		pressureReserve = grantedHeadroom
	}
	budgetAfterReserve, _ := saturatingSub(budget, pressureReserve)
	memoryMax, maxOverflow := saturatingAdd(capacity, overhead)
	high, highOverflow := saturatingAdd(overhead, budgetAfterReserve)
	if high > memoryMax {
		high = memoryMax
	}
	if hostMemoryCurrent > high {
		high = hostMemoryCurrent
		if high > memoryMax {
			high = memoryMax
		}
	}
	result := MemoryHighResult{
		MemoryMax: memoryMax, GrantedHeadroom: grantedHeadroom,
		PressureReserve: pressureReserve, HostMemoryHigh: high,
	}
	if maxOverflow || highOverflow {
		return result, ErrMemoryOverflow
	}
	return result, nil
}

// ValidateBalloonSize checks CH's uint32-PFN representation.
func ValidateBalloonSize(capacity, balloon uint64) error {
	if capacity == 0 || capacity%balloonPageSize != 0 {
		return fmt.Errorf("memory capacity %d is not positive %d-byte-page aligned", capacity, balloonPageSize)
	}
	if balloon > capacity {
		return fmt.Errorf("balloon size %d exceeds capacity %d", balloon, capacity)
	}
	if balloon%balloonPageSize != 0 {
		return fmt.Errorf("balloon size %d is not %d-byte-page aligned", balloon, balloonPageSize)
	}
	if balloon/balloonPageSize > math.MaxUint32 {
		return fmt.Errorf("balloon size %d exceeds CH uint32 PFN range", balloon)
	}
	return nil
}
