package util

import (
	"math"
	"testing"
)

func TestParseSizeExactDecimal(t *testing.T) {
	for _, test := range []struct {
		input string
		want  uint64
	}{
		{"4096", 4096},
		{"0.5KiB", 512},
		{"1.5GiB", 3 << 29},
		{"536875008B", 512<<20 + 4096},
		{"18446744073709551615", math.MaxUint64},
	} {
		got, err := ParseSize(test.input)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("ParseSize(%q)=%d, want %d", test.input, got, test.want)
		}
	}
}

func TestParseSizeRejectsInexactAndOverflow(t *testing.T) {
	for _, input := range []string{
		"0.1B",
		"1.0000000000000000001B",
		"18446744073709551616",
		"1e3",
		"NaN",
		"Inf",
		"-1GiB",
		"1..0GiB",
	} {
		if got, err := ParseSize(input); err == nil {
			t.Fatalf("ParseSize(%q)=%d, want error", input, got)
		}
	}
}
