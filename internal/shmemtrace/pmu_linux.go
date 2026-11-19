package shmemtrace

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

// perfProbe owns one ephemeral PMU event. It never writes tracefs, pins an
// object, changes a setting, or falls back to the global kprobe_events API.
type perfProbe struct{ fd int }

func (p *perfProbe) Close() error {
	if p == nil || p.fd < 0 {
		return nil
	}
	fd := p.fd
	p.fd = -1
	return unix.Close(fd)
}

func attachPMU(prog *ebpf.Program, ret bool) (_ *perfProbe, retErr error) {
	b, err := os.ReadFile("/sys/bus/event_source/devices/kprobe/type")
	if err != nil {
		return nil, err
	}
	typ, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return nil, err
	}
	var config uint64
	if ret {
		b, err = os.ReadFile("/sys/bus/event_source/devices/kprobe/format/retprobe")
		if err != nil {
			return nil, err
		}
		s := strings.TrimSpace(string(b))
		if !strings.HasPrefix(s, "config:") {
			return nil, fmt.Errorf("unsupported retprobe PMU format")
		}
		bit, err := strconv.ParseUint(strings.TrimPrefix(s, "config:"), 10, 6)
		if err != nil {
			return nil, err
		}
		config = uint64(1) << bit
	}
	sym, err := unix.BytePtrFromString("shmem_add_to_page_cache")
	if err != nil {
		return nil, err
	}
	attr := unix.PerfEventAttr{Size: unix.PERF_ATTR_SIZE_VER1, Type: uint32(typ), Config: config, Ext1: uint64(uintptr(unsafe.Pointer(sym))), Bits: unix.PerfBitDisabled}
	// kprobe's trace_call_bpf executes before the per-CPU perf output filter.
	// Like cilium/ebpf's pmuProbe, pid=-1/cpu=0 observes every process and CPU;
	// opening one event per CPU would redundantly invoke this BPF program.
	fd, err := unix.PerfEventOpen(&attr, -1, 0, -1, unix.PERF_FLAG_FD_CLOEXEC)
	runtime.KeepAlive(sym)
	if err != nil {
		return nil, err
	}
	p := &perfProbe{fd: fd}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, p.Close())
		}
	}()
	if err = unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_SET_BPF, prog.FD()); err != nil {
		return nil, err
	}
	if err = unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		return nil, err
	}
	return p, nil
}
