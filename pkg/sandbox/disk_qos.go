package sandbox

import (
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
)

// NewDiskIOController fixes the resolved host policy for one runtime. Cold
// start and memory restore each create one controller before serving devices;
// workload launch and slot binding never create or reset a controller.
func NewDiskIOController(limits config.DiffCOWLimits) *vhost.IOController {
	rates := func(l config.DiffCOWDiskLimits) vhost.IOLimits {
		return vhost.IOLimits{
			ReadBandwidth: l.ReadBandwidth, WriteBandwidth: l.WriteBandwidth,
			ReadIOPS: l.ReadIOPS, WriteIOPS: l.WriteIOPS,
		}
	}
	total, per := rates(limits.Aggregate), rates(limits.PerDisk)
	if total == (vhost.IOLimits{}) && per == (vhost.IOLimits{}) {
		return nil
	}
	return vhost.NewIOController(total, per)
}

// deviceIOBudgets expands logical disks in the same order as runtimeDeviceSpecs.
// An overlay's read-only base and writable upper share the same disk budget.
// Unbound slots keep their budget when a backend is subsequently published.
func deviceIOBudgets(controller *vhost.IOController, shapes []RuntimeDiskSpec) []*vhost.DiskIOBudget {
	var budgets []*vhost.DiskIOBudget
	for _, shape := range shapes {
		var budget *vhost.DiskIOBudget
		if controller != nil {
			budget = controller.NewDisk()
		}
		if shape.Overlay {
			budgets = append(budgets, budget)
		}
		budgets = append(budgets, budget)
	}
	return budgets
}
