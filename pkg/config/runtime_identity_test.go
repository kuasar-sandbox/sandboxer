package config

import (
	"strings"
	"testing"
)

func TestRuntimeContentMatches(t *testing.T) {
	digest := strings.Repeat("a", 64)
	required := "file://runtime-v1.bundle@digest:" + digest
	for _, test := range []struct {
		name, actual string
		match        bool
	}{
		{"same", required, true},
		{"renamed", "file://runtime-default.bundle@digest:" + digest, true},
		{"different-value", "file://runtime-v1.bundle@digest:" + strings.Repeat("b", 64), false},
		{"different-type", "file://runtime-v1.bundle@hmac:" + digest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := RuntimeContentMatches(required, test.actual)
			if err != nil || got != test.match {
				t.Fatalf("RuntimeContentMatches = %v, %v; want %v", got, err, test.match)
			}
		})
	}
	for _, invalid := range []string{
		"", "self", "file://runtime.bundle", "file:///host/runtime.bundle@digest:" + digest,
		"file://runtime.bundle@location:refs@digest:" + digest, "manifest://" + digest,
	} {
		for _, requiredInvalid := range []bool{false, true} {
			want, actual := required, invalid
			if requiredInvalid {
				want, actual = invalid, required
			}
			if matched, err := RuntimeContentMatches(want, actual); err == nil || matched {
				t.Fatalf("accepted invalid runtime identity %q as required=%v: %v / %v", invalid, requiredInvalid, matched, err)
			}
		}
	}
}
