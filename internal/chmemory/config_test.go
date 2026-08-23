package chmemory

import (
	"math"
	"strings"
	"testing"
)

func uint64ptr(value uint64) *uint64 { return &value }

func TestTotalSizeMatchesCloudHypervisor(t *testing.T) {
	config := Config{
		Size: 4096, HotpluggedSize: uint64ptr(8192),
		Zones: []Zone{
			{Size: 16 << 20},
			{Size: 32 << 20, HotpluggedSize: uint64ptr(64 << 20)},
		},
	}
	got, err := config.TotalSize()
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(4096 + 8192 + 16<<20 + 32<<20 + 64<<20)
	if got != want {
		t.Fatalf("TotalSize()=%d, want %d", got, want)
	}
}

func TestTotalSizeRejectsInvalidTotals(t *testing.T) {
	for _, config := range []Config{
		{},
		{Size: math.MaxUint64, Zones: []Zone{{Size: 1}}},
		{Zones: []Zone{{Size: math.MaxUint64, HotpluggedSize: uint64ptr(1)}}},
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
