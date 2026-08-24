package chmemory

import (
	"math"
	"strings"
	"testing"
)

func uint64ptr(value uint64) *uint64 { return &value }

func TestTotalSizeMatchesCloudHypervisorMemorySelection(t *testing.T) {
	zero := uint64(0)
	tests := []struct {
		name   string
		config Config
		want   uint64
	}{
		{
			name:   "top-level memory",
			config: Config{Size: 512<<20 + 4096, HotplugSize: &zero, HotpluggedSize: &zero},
			want:   512<<20 + 4096,
		},
		{
			name: "memory zones",
			config: Config{Zones: []Zone{
				{Size: 16 << 20},
				{Size: 32<<20 + 4096, HotplugSize: &zero, HotpluggedSize: &zero},
			}},
			want: 48<<20 + 4096,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.config.TotalSize()
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("TotalSize()=%d, want %d", got, tt.want)
			}
		})
	}
}

func TestTotalSizeRejectsInvalidTotals(t *testing.T) {
	for _, config := range []Config{
		{},
		{Zones: []Zone{}},
		{Size: 1, Zones: []Zone{{Size: 1}}},
		{Size: 1, HotplugSize: uint64ptr(1)},
		{Size: 1, HotpluggedSize: uint64ptr(1)},
		{Zones: []Zone{{Size: 1, HotplugSize: uint64ptr(1)}}},
		{Zones: []Zone{{Size: 1, HotpluggedSize: uint64ptr(1)}}},
		{Zones: []Zone{{Size: math.MaxUint64}, {Size: 1}}},
	} {
		if _, err := config.TotalSize(); err == nil {
			t.Fatalf("TotalSize(%+v) accepted invalid total", config)
		}
	}
}

func TestCapacityFromVMConfig(t *testing.T) {
	got, err := CapacityFromVMConfig([]byte(`{
		"memory": {
			"size": 0,
			"zones": [{"id":"ram0","size":536875008}]
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(512<<20 + 4096); got != want {
		t.Fatalf("CapacityFromVMConfig()=%d, want %d", got, want)
	}
}

func TestCapacityFromVMConfigRejectsMalformedOrMissingMemory(t *testing.T) {
	for _, input := range []string{`{`, `{}`, `null`, `{"memory":null}`, `{"memory":{"size":0}}`} {
		if _, err := CapacityFromVMConfig([]byte(input)); err == nil || !strings.Contains(err.Error(), "config.json") {
			t.Fatalf("CapacityFromVMConfig(%q) error=%v, want config.json error", input, err)
		}
	}
}
