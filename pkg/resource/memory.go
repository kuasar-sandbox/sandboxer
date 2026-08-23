package resource

import (
	"fmt"
	"math"
	"math/big"
)

// MemoryStep is the single step used by the sandbox-local Budget calculation
// and balloon executor. The reservation adapter only carries the resulting
// absolute Budget and delta; the node does not choose this step.
const MemoryStep uint64 = 64 << 20

// RatioScale is the fixed-point denominator used for memory.high ratios.
const RatioScale uint64 = 1_000_000

// DefaultWatermarkHighRatio represents 0.875 at RatioScale precision.
const DefaultWatermarkHighRatio uint64 = 875_000

// RatioFromFloat converts a validated YAML float to a conservative fixed-point
// ratio. Flooring the ratio can only retain a larger pressure reserve. A
// positive value below one fixed-point unit is represented by zero; callers
// validate the external 0 < ratio < 1 contract here, while the Budget formula
// accepts zero as that conservative quantized representation.
func RatioFromFloat(ratio float64) (uint64, error) {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 || ratio >= 1 {
		return 0, fmt.Errorf("memory.high ratio must be between 0 and 1")
	}
	r := new(big.Rat).SetFloat64(ratio)
	numerator := new(big.Int).Mul(r.Num(), new(big.Int).SetUint64(RatioScale))
	fixed := new(big.Int).Quo(numerator, r.Denom())
	if !fixed.IsUint64() {
		return 0, fmt.Errorf("memory.high ratio cannot be represented")
	}
	value := fixed.Uint64()
	if value >= RatioScale {
		return 0, fmt.Errorf("memory.high ratio must be between 0 and 1")
	}
	return value, nil
}
