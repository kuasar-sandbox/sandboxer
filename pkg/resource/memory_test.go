package resource

import (
	"math"
	"testing"
)

func TestRatioFromFloat(t *testing.T) {
	got, err := RatioFromFloat(0.875)
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultWatermarkHighRatio {
		t.Fatalf("RatioFromFloat(0.875)=%d, want %d", got, DefaultWatermarkHighRatio)
	}
	if got, err := RatioFromFloat(0.0000001); err != nil || got != 0 {
		t.Fatalf("RatioFromFloat(sub-precision)=(%d,%v), want conservative zero", got, err)
	}
	for _, value := range []float64{0, 1, -1, math.NaN(), math.Inf(1)} {
		if _, err := RatioFromFloat(value); err == nil {
			t.Fatalf("RatioFromFloat(%v) succeeded", value)
		}
	}
}
