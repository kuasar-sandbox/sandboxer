package config

import (
	"fmt"

	"github.com/kuasar-sandbox/sandboxer/pkg/util"
	"gopkg.in/yaml.v3"
)

// DiffCOWDiskLimits is a resolved zero-means-unlimited policy. Bandwidth uses
// bytes per second; IOPS counts complete logical requests. Dirty bytes remain
// part of the same shared cache, including when a disk-specific limit is set.
type DiffCOWDiskLimits struct {
	MaxDirtySize, ReadBandwidth, WriteBandwidth, ReadIOPS, WriteIOPS uint64
}

// DiffCOWLimits contains a shared aggregate limit and the uniform independent
// limit for each logical disk. A request must satisfy both rate limits.
type DiffCOWLimits struct {
	Aggregate DiffCOWDiskLimits
	PerDisk   DiffCOWDiskLimits
}

func cloneDiffCOWUint64(p *uint64) *uint64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Clone retains omitted-versus-zero presence without sharing mutable leaves.
func (c DiffCOWConfig) Clone() DiffCOWConfig {
	c.ReadIOPS = cloneDiffCOWUint64(c.ReadIOPS)
	c.WriteIOPS = cloneDiffCOWUint64(c.WriteIOPS)
	c.ReadIOPSPerDisk = cloneDiffCOWUint64(c.ReadIOPSPerDisk)
	c.WriteIOPSPerDisk = cloneDiffCOWUint64(c.WriteIOPSPerDisk)
	return c
}

func diffCOWDirtySize(field, value string) (uint64, error) {
	n, err := util.ParseSize(value)
	if err != nil || n%4096 != 0 || n > uint64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("resources.diff_cow.%s must be zero or a positive 4 KiB multiple fitting host address space (got %q)", field, value)
	}
	return n, nil
}

func diffCOWBandwidth(field, value string) (uint64, error) {
	n, err := util.ParseSize(value)
	if err != nil {
		return 0, fmt.Errorf("resources.diff_cow.%s must be a nonnegative whole-byte rate (got %q): %w", field, value, err)
	}
	return n, nil
}

func (c DiffCOWConfig) validateQOS() error {
	if c.MaxDirtySizePerDisk != "" {
		if _, err := diffCOWDirtySize("max_dirty_size_per_disk", c.MaxDirtySizePerDisk); err != nil {
			return err
		}
	}
	for _, item := range []struct{ field, value string }{
		{"read_bandwidth", c.ReadBandwidth}, {"write_bandwidth", c.WriteBandwidth},
		{"read_bandwidth_per_disk", c.ReadBandwidthPerDisk}, {"write_bandwidth_per_disk", c.WriteBandwidthPerDisk},
	} {
		if item.value != "" {
			if _, err := diffCOWBandwidth(item.field, item.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Limits resolves after all configuration overlays. No pointers or strings are
// needed in the I/O path; zero stays the sentinel for an absent rate ceiling.
func (c DiffCOWConfig) Limits() (DiffCOWLimits, error) {
	_, dirty, err := c.sizes(true)
	if err != nil {
		return DiffCOWLimits{}, err
	}
	parse := func(s string) uint64 {
		if s == "" {
			return 0
		}
		n, _ := util.ParseSize(s) // validated by sizes
		return n
	}
	value := func(p *uint64) uint64 {
		if p == nil {
			return 0
		}
		return *p
	}
	return DiffCOWLimits{
		Aggregate: DiffCOWDiskLimits{dirty, parse(c.ReadBandwidth), parse(c.WriteBandwidth), value(c.ReadIOPS), value(c.WriteIOPS)},
		PerDisk:   DiffCOWDiskLimits{parse(c.MaxDirtySizePerDisk), parse(c.ReadBandwidthPerDisk), parse(c.WriteBandwidthPerDisk), value(c.ReadIOPSPerDisk), value(c.WriteIOPSPerDisk)},
	}, nil
}

func (c *DiffCOWConfig) decodeQOSLeaf(key string, val *yaml.Node) (bool, error) {
	var size *string
	var iops **uint64
	parse := diffCOWBandwidth
	switch key {
	case "max_dirty_size_per_disk":
		size = &c.MaxDirtySizePerDisk
		parse = diffCOWDirtySize
	case "read_bandwidth":
		size = &c.ReadBandwidth
	case "write_bandwidth":
		size = &c.WriteBandwidth
	case "read_bandwidth_per_disk":
		size = &c.ReadBandwidthPerDisk
	case "write_bandwidth_per_disk":
		size = &c.WriteBandwidthPerDisk
	case "read_iops":
		iops = &c.ReadIOPS
	case "write_iops":
		iops = &c.WriteIOPS
	case "read_iops_per_disk":
		iops = &c.ReadIOPSPerDisk
	case "write_iops_per_disk":
		iops = &c.WriteIOPSPerDisk
	default:
		return false, nil
	}
	if size != nil {
		if val.Kind != yaml.ScalarNode || val.Tag == "!!null" {
			return true, fmt.Errorf("resources.diff_cow.%s must be a size", key)
		}
		if _, err := parse(key, val.Value); err != nil {
			return true, err
		}
		*size = val.Value
		return true, nil
	}
	if val.Kind != yaml.ScalarNode || val.Tag != "!!int" {
		return true, fmt.Errorf("resources.diff_cow.%s must be a nonnegative integer", key)
	}
	var n uint64
	if err := val.Decode(&n); err != nil {
		return true, fmt.Errorf("resources.diff_cow.%s must be a nonnegative integer: %w", key, err)
	}
	*iops = &n
	return true, nil
}
