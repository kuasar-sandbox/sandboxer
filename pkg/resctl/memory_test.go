package resctl

import (
	"errors"
	"math"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
)

func TestCalculateMemoryBudget(t *testing.T) {
	const (
		capacity = 8 << 30
		headroom = 256 << 20
	)
	for _, tt := range []struct {
		name                                  string
		target, current, available            uint64
		wantDemand, wantRequest, wantObserved uint64
	}{
		{"stable", 4 << 30, 4 << 30, 512 << 20, 3584 << 20, 3840 << 20, 4 << 30},
		{"inflate pending", 5 << 30, 4 << 30, 512 << 20, 3584 << 20, 3840 << 20, 4 << 30},
		{"deflate pending", 3 << 30, 4 << 30, 512 << 20, 3584 << 20, 3840 << 20, 5 << 30},
		{"available exceeds budget", 7 << 30, 1 << 30, 2 << 30, 0, 256 << 20, 1 << 30},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CalculateMemoryBudget(capacity, headroom, tt.target, tt.current, tt.available)
			if err != nil {
				t.Fatal(err)
			}
			if got.DemandMemory != tt.wantDemand || got.RequestedBudget != tt.wantRequest || got.ObservedBudget != tt.wantObserved {
				t.Fatalf("result=%+v, want demand=%d request=%d observed=%d", got, tt.wantDemand, tt.wantRequest, tt.wantObserved)
			}
			if got.RequestedBudget < got.DemandMemory+headroom {
				t.Fatalf("alignment lost headroom: %+v", got)
			}
		})
	}
}

func TestMemoryBudgetAlignmentAndBounds(t *testing.T) {
	capacity := uint64(1<<30 + 4096)
	budget := uint64(256<<20 + 1)
	target := TargetForBudget(capacity, budget)
	if target%resource.MemoryStep != 0 {
		t.Fatalf("target %d is not aligned", target)
	}
	if got := BudgetFromTarget(capacity, target); got < budget {
		t.Fatalf("aligned budget %d is below request %d", got, budget)
	}
	if got := TargetForBudget(capacity, math.MaxUint64); got != 0 {
		t.Fatalf("full budget target=%d, want 0", got)
	}
	if _, err := CalculateMemoryBudget(capacity, 1, capacity+1, 0, 0); err == nil {
		t.Fatal("accepted target above capacity")
	}
	if _, err := CalculateMemoryBudget(capacity, 1, 0, capacity+1, 0); err == nil {
		t.Fatal("accepted current Budget above capacity")
	}
}

func TestAlignedBudgetAtMostReservationNeverOvercommitsPartialGrant(t *testing.T) {
	const capacity = uint64(1 << 30)
	for _, tc := range []struct {
		reservation uint64
		want        uint64
	}{
		{reservation: 512 << 20, want: 512 << 20},
		{reservation: 562 << 20, want: 512 << 20},
		{reservation: 576 << 20, want: 576 << 20},
		{reservation: 640 << 20, want: 640 << 20},
		{reservation: capacity, want: capacity},
		{reservation: math.MaxUint64, want: capacity},
	} {
		got := alignedBudgetAtMostReservation(capacity, tc.reservation)
		if got != tc.want {
			t.Fatalf("alignedBudgetAtMostReservation(%d) = %d, want %d", tc.reservation, got, tc.want)
		}
		if got > tc.reservation {
			t.Fatalf("aligned Budget %d exceeds reservation %d", got, tc.reservation)
		}
	}
}

func TestAlignedBudgetAtMostReservationWithCapacityRemainder(t *testing.T) {
	const capacity = uint64(1<<30 + 4096)
	for _, tc := range []struct {
		reservation uint64
		want        uint64
	}{
		{reservation: 0, want: 0},
		{reservation: 4095, want: 0},
		{reservation: 4096, want: 4096},
		{reservation: 64 << 20, want: 4096},
		{reservation: 64<<20 + 4096, want: 64<<20 + 4096},
		{reservation: capacity - 1, want: capacity - resource.MemoryStep},
		{reservation: capacity, want: capacity},
	} {
		got := alignedBudgetAtMostReservation(capacity, tc.reservation)
		if got != tc.want {
			t.Fatalf("alignedBudgetAtMostReservation(%d) = %d, want %d", tc.reservation, got, tc.want)
		}
		if got > tc.reservation {
			t.Fatalf("aligned Budget %d exceeds reservation %d", got, tc.reservation)
		}
		if got > 0 && BudgetFromTarget(capacity, TargetForBudget(capacity, got)) != got {
			t.Fatalf("Budget %d is not exactly target-representable", got)
		}
	}
}

func TestMemoryBudgetOverflowIsConservativeError(t *testing.T) {
	got, err := CalculateMemoryBudget(math.MaxUint64, math.MaxUint64, 0, math.MaxUint64, 0)
	if !errors.Is(err, ErrMemoryOverflow) {
		t.Fatalf("error=%v, want overflow", err)
	}
	if got.RequestedBudget != math.MaxUint64 || got.DesiredTarget != 0 {
		t.Fatalf("overflow result=%+v, want clamped full Capacity", got)
	}
}

func TestNextShrinkTargetStableOneStep(t *testing.T) {
	const capacity = uint64(1 << 30)
	for _, tt := range []struct {
		name                     string
		target, current, request uint64
		want                     uint64
		ok                       bool
	}{
		{"more than deadband", 256 << 20, 768 << 20, 640 << 20, 320 << 20, true},
		{"unstable", 256 << 20, 704 << 20, 512 << 20, 256 << 20, false},
		{"exactly one step deadband", 256 << 20, 768 << 20, 704 << 20, 256 << 20, false},
		{"inside deadband", 256 << 20, 768 << 20, 735 << 20, 256 << 20, false},
		{"grow direction", 256 << 20, 768 << 20, 896 << 20, 256 << 20, false},
		{"unaligned restore target", 100 << 20, 924 << 20, 700 << 20, 128 << 20, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NextShrinkTarget(capacity, tt.target, tt.current, tt.request)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("NextShrinkTarget()=(%d,%v), want (%d,%v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNextShrinkTargetCapacityRemainderPreservesStepBounds(t *testing.T) {
	const capacity = uint64(1<<30 + 4096)
	const accepted = uint64(100 << 20)
	current := BudgetFromTarget(capacity, accepted)
	next, ok := NextShrinkTarget(capacity, accepted, current, 700<<20)
	if !ok {
		t.Fatal("non-step restored target did not make shrink progress")
	}
	if next <= accepted || next-accepted > resource.MemoryStep || next%resource.MemoryStep != 0 {
		t.Fatalf("next target=%d from accepted=%d violates one-step/alignment bounds", next, accepted)
	}
	if got := BudgetFromTarget(capacity, next); got >= current {
		t.Fatalf("next Budget=%d did not shrink current Budget=%d", got, current)
	}
}

func TestCalculateMemoryHigh(t *testing.T) {
	got, err := CalculateMemoryHigh(8<<30, 128<<20, 4<<30, 3840<<20, resource.DefaultWatermarkHighRatio, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.GrantedHeadroom != 256<<20 || got.PressureReserve != 64<<20 || got.HostMemoryHigh != 4160<<20 {
		t.Fatalf("result=%+v", got)
	}
	got, err = CalculateMemoryHigh(8<<30, 128<<20, 4<<30, 3840<<20, resource.DefaultWatermarkHighRatio, 5<<30)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostMemoryHigh != 5<<30 {
		t.Fatalf("host charge safety floor=%d, want %d", got.HostMemoryHigh, uint64(5<<30))
	}
}

func TestCalculateMemoryHighBoundaries(t *testing.T) {
	const (
		capacity = uint64(1 << 30)
		overhead = uint64(64 << 20)
	)
	zeroHeadroom, err := CalculateMemoryHigh(capacity, overhead, 512<<20, 512<<20,
		resource.DefaultWatermarkHighRatio, 0)
	if err != nil {
		t.Fatal(err)
	}
	if zeroHeadroom.GrantedHeadroom != 0 || zeroHeadroom.PressureReserve != 0 ||
		zeroHeadroom.HostMemoryHigh != 576<<20 {
		t.Fatalf("zero-headroom result=%+v", zeroHeadroom)
	}

	subStep, err := CalculateMemoryHigh(capacity, overhead, 512<<20, 480<<20,
		resource.DefaultWatermarkHighRatio, 0)
	if err != nil {
		t.Fatal(err)
	}
	if subStep.GrantedHeadroom != 32<<20 || subStep.PressureReserve != 32<<20 ||
		subStep.HostMemoryHigh != 544<<20 {
		t.Fatalf("sub-step headroom result=%+v", subStep)
	}

	quantizedZero, err := CalculateMemoryHigh(capacity, overhead, 512<<20, 256<<20,
		0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if quantizedZero.PressureReserve != 256<<20 || quantizedZero.HostMemoryHigh != 320<<20 {
		t.Fatalf("sub-precision ratio result=%+v", quantizedZero)
	}

	hostClamped, err := CalculateMemoryHigh(capacity, overhead, 512<<20, 256<<20,
		resource.DefaultWatermarkHighRatio, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	if hostClamped.HostMemoryHigh != capacity+overhead {
		t.Fatalf("host charge clamp=%d, want memory.max=%d", hostClamped.HostMemoryHigh, capacity+overhead)
	}

	overflow, err := CalculateMemoryHigh(math.MaxUint64, 1, math.MaxUint64, math.MaxUint64,
		resource.DefaultWatermarkHighRatio, 0)
	if !errors.Is(err, ErrMemoryOverflow) {
		t.Fatalf("overflow error=%v, want ErrMemoryOverflow", err)
	}
	if overflow.MemoryMax != math.MaxUint64 || overflow.HostMemoryHigh != math.MaxUint64 {
		t.Fatalf("overflow result=%+v, want conservative saturation", overflow)
	}
}

func TestCalculateMemoryBudgetInvariants(t *testing.T) {
	capacities := []uint64{resource.MemoryStep, resource.MemoryStep + 4096, 1<<30 + 4096}
	for _, capacity := range capacities {
		headrooms := []uint64{4096, min(resource.MemoryStep, capacity), capacity}
		targets := []uint64{0, min(resource.MemoryStep, capacity), alignDown(capacity, resource.MemoryStep)}
		for _, headroom := range headrooms {
			for _, target := range targets {
				if target > capacity {
					continue
				}
				for _, current := range []uint64{0, capacity / 2, capacity} {
					for _, available := range []uint64{0, current / 2, current, capacity} {
						got, err := CalculateMemoryBudget(capacity, headroom, target, current, available)
						if err != nil {
							t.Fatalf("C=%d H=%d T=%d A=%d M=%d: %v", capacity, headroom, target, current, available, err)
						}
						wantDemand := uint64(0)
						if current > available {
							wantDemand = current - available
						}
						if got.DemandMemory != wantDemand || got.TargetBudget != BudgetFromTarget(capacity, target) {
							t.Fatalf("formula mismatch: %+v", got)
						}
						if got.DesiredTarget%resource.MemoryStep != 0 || got.RequestedBudget > capacity ||
							got.RequestedBudget < got.RawRequested {
							t.Fatalf("alignment/bounds mismatch: %+v", got)
						}
						wantObserved := max(got.TargetBudget, current)
						if got.ObservedBudget != wantObserved {
							t.Fatalf("ObservedBudget=%d, want %d: %+v", got.ObservedBudget, wantObserved, got)
						}
					}
				}
			}
		}
	}
}

func TestValidateBalloonSize(t *testing.T) {
	const capacity = uint64(8 << 30)
	if err := ValidateBalloonSize(capacity, 64<<20); err != nil {
		t.Fatal(err)
	}
	for _, value := range []uint64{capacity + 4096, 1} {
		if err := ValidateBalloonSize(capacity, value); err == nil {
			t.Fatalf("ValidateBalloonSize(%d) succeeded", value)
		}
	}
	tooLarge := (uint64(math.MaxUint32) + 1) << 12
	if err := ValidateBalloonSize(tooLarge, tooLarge); err == nil {
		t.Fatal("accepted balloon outside uint32 PFN range")
	}
}
