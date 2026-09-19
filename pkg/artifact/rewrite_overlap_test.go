package artifact

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseRewriteOptionsRejectsEveryReplacementOverlap(t *testing.T) {
	a, b, c, d := rewriteTestRef('a'), rewriteTestRef('b'), rewriteTestRef('c'), rewriteTestRef('d')
	tests := []struct {
		name                     string
		replacements, reductions []string
	}{
		{"duplicate", []string{a + "=" + b, a + "=" + b}, nil},
		{"same-source", []string{a + "=" + b, a + "=" + c}, nil},
		{"same-target", []string{a + "=" + c, b + "=" + c}, nil},
		{"chain", []string{a + "=" + b, b + "=" + c}, nil},
		{"cycle", []string{a + "=" + b, b + "=" + a}, nil},
		{"same-reduction", []string{a + "=" + b}, []string{a + "=" + b}},
		{"different-reduction", []string{a + "=" + b}, []string{a + "=" + c}},
		{"automatic-top", []string{a + "=" + b}, []string{a}},
		{"replacement-target-is-top", []string{a + "=" + b}, []string{b}},
		{"reduction-target-is-source", []string{a + "=" + b}, []string{c + "=" + a}},
		{"reduction-target-is-target", []string{a + "=" + b}, []string{c + "=" + b}},
		{"any", []string{a + "=" + b}, []string{"any"}},
	}
	for _, tt := range tests {
		for _, skip := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/skip=%t/reverse=%t", tt.name, skip, reverse), func(t *testing.T) {
					replacements := append([]string(nil), tt.replacements...)
					if reverse {
						for i, j := 0, len(replacements)-1; i < j; i, j = i+1, j-1 {
							replacements[i], replacements[j] = replacements[j], replacements[i]
						}
					}
					_, err := ParseRewriteOptions(replacements, tt.reductions, skip)
					if err == nil || !strings.Contains(err.Error(), "overlap") {
						t.Fatalf("want overlap rejection, got %v", err)
					}
				})
			}
		}
	}
	// Independent references remain available in the same publication.
	for _, skip := range []bool{false, true} {
		if _, err := ParseRewriteOptions([]string{a + "=" + b}, []string{c + "=" + d}, skip); err != nil {
			t.Fatalf("independent rules: %v", err)
		}
	}
}
