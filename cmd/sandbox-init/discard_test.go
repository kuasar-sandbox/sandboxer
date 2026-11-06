package main

import (
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func TestExt4DiscardMountPolicy(t *testing.T) {
	for _, tc := range []struct {
		mode, options string
	}{
		{"", "discard"},
		{proto.DiscardAuto, "discard"},
		{proto.DiscardManual, "nodiscard"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			got, err := ext4MountOptions(tc.mode)
			if err != nil || got != tc.options {
				t.Fatalf("ext4MountOptions(%q) = %q, %v; want %q", tc.mode, got, err, tc.options)
			}
		})
	}
	for _, mode := range []string{"off", "AUTO", " manual", "manual ", "discard,nodiscard"} {
		t.Run(mode, func(t *testing.T) {
			// Invalid policy must fail before probing or mounting any disk.
			if err := phaseWorkloadRoot(&proto.LaunchSpec{Ext4Discard: mode}); err == nil || !strings.Contains(err.Error(), "ext4_discard") {
				t.Fatalf("invalid policy reached workload assembly: %v", err)
			}
		})
	}
}
