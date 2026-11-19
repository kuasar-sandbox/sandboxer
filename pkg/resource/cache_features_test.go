package resource

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCacheBudgetLeaseRetainsStrictLegacySchema(t *testing.T) {
	const maximum = uint64(1 << 30)
	l := Lease{
		Version: LeaseVersion, SandboxID: "old-controller-compatible", PID: 333,
		ControllerSocket: "/run/sandbox-resource.sock", CgroupPath: "/sandbox/test",
		CapacityMemory: 1 << 30, CapacityCPUMilli: 1000,
		FloorMemory: 256 << 20, FloorCPUMilli: 1000,
		StartupMemory: 512 << 20,
		ClientFeatures: []string{FeatureStateSyncV1, FeatureCacheBudgetV1, CacheMaximumFeature(maximum),
			FeatureCacheReclaimV1, CacheReclaimRunFeature(testCacheReclaimRun)},
	}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	// Earlier nodes decode leases with DisallowUnknownFields. The new bound
	// travels in the already-present client_features string list only.
	var legacy struct {
		Version          int      `json:"version"`
		SandboxID        string   `json:"sandbox_id"`
		PID              int      `json:"pid"`
		ControllerSocket string   `json:"controller_socket"`
		CgroupPath       string   `json:"cgroup_path"`
		CapacityMemory   uint64   `json:"capacity_memory"`
		CapacityCPUMilli uint64   `json:"capacity_cpu_milli"`
		FloorMemory      uint64   `json:"floor_memory"`
		FloorCPUMilli    uint64   `json:"floor_cpu_milli"`
		StartupMemory    uint64   `json:"startup_memory"`
		ClientFeatures   []string `json:"client_features,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&legacy); err != nil {
		t.Fatalf("legacy lease reader rejected new feature: %v, %s", err, b)
	}
	if !HasFeature(legacy.ClientFeatures, FeatureCacheBudgetV1) {
		t.Fatal("cache feature lost in legacy lease")
	}
	got, err := CacheMaximumFromFeatures(legacy.ClientFeatures)
	if err != nil || got != maximum {
		t.Fatalf("lost cache recovery bound: %d %v", got, err)
	}
	for _, fs := range [][]string{
		{FeatureCacheBudgetV1},
		{CacheMaximumFeature(maximum)},
		{FeatureCacheBudgetV1, CacheMaximumFeature(maximum), CacheMaximumFeature(maximum)},
		{FeatureCacheBudgetV1, CacheMaximumFeature(42)},
	} {
		if _, err := CacheMaximumFromFeatures(fs); err == nil {
			t.Fatalf("accepted invalid immutable cache feature contract: %q", fs)
		}
	}
}
