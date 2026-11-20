package sandbox

import (
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

func TestDeviceIOBudgetsMatchLogicalDiskLayout(t *testing.T) {
	// Include an overlay root, ordinary writable disk, and an empty slot: a
	// frontend ordinal is not the logical disk ordinal in either cold or restore.
	shapes := []RuntimeDiskSpec{
		{Overlay: true, BaseCapacity: 4096, WritableCapacity: 4096},
		{Name: "workspace", WritableCapacity: 4096},
		{Name: "later", Slot: true},
	}
	devices, err := runtimeDeviceSpecs(shapes)
	if err != nil {
		t.Fatal(err)
	}
	controller := NewDiskIOController(config.DiffCOWLimits{PerDisk: config.DiffCOWDiskLimits{ReadIOPS: 1}})
	budgets := deviceIOBudgets(controller, shapes)
	if len(budgets) != len(devices) || len(budgets) != 4 {
		t.Fatalf("budget/device layout: %d/%d", len(budgets), len(devices))
	}
	if budgets[0] == nil || budgets[0] != budgets[1] {
		t.Fatal("overlay lower/upper must share one logical disk budget")
	}
	if budgets[2] == nil || budgets[3] == nil || budgets[2] == budgets[0] || budgets[3] == budgets[0] || budgets[2] == budgets[3] {
		t.Fatal("writable disk and unbound slot must retain independent budgets")
	}
	// A different runtime never borrows the same per-disk accounting state.
	other := deviceIOBudgets(controller, shapes)
	if other[0] == budgets[0] {
		t.Fatal("logical disk budget reused across frontend sets")
	}
}

func TestDiskIOControllerUnrestrictedFastPath(t *testing.T) {
	limits := config.DiffCOWLimits{
		Aggregate: config.DiffCOWDiskLimits{MaxDirtySize: 4096},
		PerDisk:   config.DiffCOWDiskLimits{MaxDirtySize: 8192},
	}
	if controller := NewDiskIOController(limits); controller != nil {
		t.Fatal("dirty-only policy enabled request rate accounting")
	}
	budgets := deviceIOBudgets(nil, []RuntimeDiskSpec{{Overlay: true}, {Slot: true}})
	if len(budgets) != 3 {
		t.Fatal("unrestricted policy lost frontend ordinals")
	}
	for _, budget := range budgets {
		if budget != (*vhost.DiskIOBudget)(nil) {
			t.Fatal("unrestricted frontend acquired rate budget")
		}
	}
}
