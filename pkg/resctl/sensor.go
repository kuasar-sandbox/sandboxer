package resctl

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
	"golang.org/x/sys/unix"
)

// Sensor mode constants. See config.SensorConfig in config.go.
const (
	SensorModePSI        = "psi"
	SensorModeEventsPoll = "events_poll"
	SensorModeNone       = "none"
)

// PressureSensor watches the per-sandbox cgroup for memory pressure and asks
// the sandbox-local MemoryController for one-step grow. Only that controller
// decides whether a node reservation request and balloon deflate are needed.
//
// Two data sources are supported (selected by config.SensorConfig.Mode):
//
//   - "psi" (default): epoll on cgroup memory.pressure with a "some"
//     trigger. The kernel wakes the sensor as soon as accumulated
//     in-window stall crosses the threshold (sub-millisecond reaction
//     after the trigger fires). On wake, sensor issues
//     RequestBudget(urgency=normal). A 1s sidecar ticker still polls
//     memory.events.local so OOM events drive urgency=high
//     (PSI by itself does not signal cgroup OOM transitions).
//
//   - "events_poll": legacy 100ms poll of memory.events.local. Used
//     when the kernel rejects the PSI trigger write (older kernels
//     without PSI trigger support).
//
// Both modes funnel through the same dispatch() → local Budget transaction.
type PressureSensor struct {
	controller *MemoryController
	cgroupPath string
	logf       func(string, ...any)
	runtime    sensorRuntime
}

type sensorRuntime struct {
	Mode        string
	StallUs     uint64
	WindowUs    uint64
	MinInterval time.Duration
}

// NewPressureSensor builds a sensor pinned to a cgroup directory. Mode / PSI
// trigger / debounce are resolved from cfg; MemoryStep is the sole grow step.
func NewPressureSensor(controller *MemoryController, cfg *config.SandboxConfig, cgroupPath string, logf func(string, ...any)) *PressureSensor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mode, stall, win, dbnc := cfg.SensorRuntime()
	return &PressureSensor{
		controller: controller,
		cgroupPath: cgroupPath,
		logf:       logf,
		runtime: sensorRuntime{
			Mode:        mode,
			StallUs:     stall,
			WindowUs:    win,
			MinInterval: dbnc,
		},
	}
}

// Run is the per-sandbox sensor goroutine entry point. Selects the
// configured mode; psi mode auto-falls-back to events_poll if PSI
// trigger setup fails (older kernel without trigger support).
//
// Returns when ctx is cancelled. It starts only after the cold launch ACK or
// restore ACK+MUX barrier; the node's independent Settled notification is not
// a prerequisite for sandbox-local pressure handling.
func (s *PressureSensor) Run(ctx context.Context) {
	if s == nil || s.controller == nil {
		return
	}
	switch s.runtime.Mode {
	case SensorModeNone:
		return
	case SensorModeEventsPoll:
		s.runEventsPoll(ctx)
		return
	}
	if s.cgroupPath == "" {
		// no-cgroup mode — no memory.pressure / memory.events to read.
		return
	}
	// Default + explicit "psi": try PSI, fall back to events_poll.
	if err := s.runPSI(ctx); err != nil {
		s.logf("sensor: PSI mode setup failed (%v); falling back to events_poll", err)
		s.runEventsPoll(ctx)
	}
}

// runPSI sets up a PSI trigger on memory.pressure and reacts via epoll.
// A sidecar goroutine concurrently polls memory.events.local at 1s for
// OOM events (PSI doesn't signal cgroup OOM transitions).
//
// Returns non-nil error only on PSI trigger setup failure (caller
// falls back to events_poll). Normal ctx cancel returns nil.
func (s *PressureSensor) runPSI(ctx context.Context) error {
	pressPath := filepath.Join(s.cgroupPath, "memory.pressure")
	pf, err := os.OpenFile(pressPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", pressPath, err)
	}
	defer pf.Close()

	trig := fmt.Sprintf("some %d %d", s.runtime.StallUs, s.runtime.WindowUs)
	if _, err := pf.Write([]byte(trig)); err != nil {
		return fmt.Errorf("write trigger %q: %w", trig, err)
	}

	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	defer unix.Close(epfd)

	// PSI triggers signal via EPOLLPRI. Level-triggered: we drain after
	// each wake by reading the file (which returns current pressure
	// values; we discard them — the wakeup is the signal).
	pfd := int(pf.Fd())
	ev := &unix.EpollEvent{Events: unix.EPOLLPRI, Fd: int32(pfd)}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, pfd, ev); err != nil {
		return fmt.Errorf("epoll_ctl ADD: %w", err)
	}

	s.logf("sensor: PSI mode active (trigger=%q debounce=%s step=%d)",
		trig, s.runtime.MinInterval, resource.MemoryStep)

	// OOM sidecar.
	sideCtx, cancelSide := context.WithCancel(ctx)
	defer cancelSide()
	go s.runOOMSidecar(sideCtx)

	events := make([]unix.EpollEvent, 1)
	drain := make([]byte, 256)
	var lastDispatch time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		// 100ms timeout bounds ctx-cancel latency.
		n, err := unix.EpollWait(epfd, events, 100)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			// Don't fall back — this is a runtime failure, not a setup
			// failure. Log and exit (Run returns; sensor is dead for
			// the rest of the sandbox's life).
			s.logf("sensor: epoll_wait: %v (sensor exiting)", err)
			return nil
		}
		if n == 0 {
			continue
		}
		// Drain the pressure file so level-triggered epoll quiesces.
		// Read returns the current "some/full avg10/60/300 total" line
		// pair — we discard it; the wakeup itself is the signal.
		if _, rerr := unix.Read(pfd, drain); rerr != nil && !errors.Is(rerr, unix.EAGAIN) {
			// Read failure is unusual; log + continue (next epoll wake
			// may still work).
			s.logf("sensor: drain memory.pressure: %v", rerr)
		}
		// Re-seek to file start so next Read (if level-triggered re-arms)
		// reads fresh data, not zero-length EOF.
		if _, serr := pf.Seek(0, 0); serr != nil {
			s.logf("sensor: seek memory.pressure: %v", serr)
		}

		now := time.Now()
		if now.Sub(lastDispatch) < s.runtime.MinInterval {
			continue
		}
		lastDispatch = now
		s.dispatch(resource.UrgencyNormal, "psi_some")
	}
}

// runOOMSidecar polls memory.events.local at 1s for OOM transitions
// (cgroup OOM events are not surfaced via PSI triggers). On dOOM > 0,
// fires urgency=high. dHigh increases are logged-only — when PSI is
// the primary path, high counter rising without a PSI wake means PSI
// missed something (rare; useful for tuning the trigger threshold).
func (s *PressureSensor) runOOMSidecar(ctx context.Context) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	var lastOOM, lastHigh uint64
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		high, oom, ok := readMemoryEvents(s.cgroupPath)
		if !ok {
			continue
		}
		// On first tick just seed lastOOM/lastHigh — any pre-existing
		// counts are from before this sensor came up.
		if first {
			lastOOM = oom
			lastHigh = high
			first = false
			continue
		}
		if oom > lastOOM {
			s.dispatch(resource.UrgencyHigh, "oom_event")
		}
		if high > lastHigh {
			s.logf("sensor: memory.events.high counter rose by %d while PSI is primary "+
				"(consider tightening psi_some_stall_us)", high-lastHigh)
		}
		lastOOM = oom
		lastHigh = high
	}
}

// runEventsPoll is the legacy 100ms poll path. Used as fallback when
// PSI trigger setup fails OR when config.SensorConfig.Mode is "events_poll".
func (s *PressureSensor) runEventsPoll(ctx context.Context) {
	s.logf("sensor: events_poll mode active (100ms tick step=%d)", resource.MemoryStep)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var lastHigh, lastOOM, previousHostCharge uint64
	var prevAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			high, oom, ok := readMemoryEvents(s.cgroupPath)
			if !ok {
				continue
			}
			hostCharge := readHostMemoryChargeBestEffort(s.cgroupPath)
			memHigh := readUintFromCgFile(s.cgroupPath, "memory.high")

			dHigh := uint64(0)
			dOOM := uint64(0)
			if high > lastHigh {
				dHigh = high - lastHigh
			}
			if oom > lastOOM {
				dOOM = oom - lastOOM
			}
			lastHigh = high
			lastOOM = oom

			rising := false
			if !prevAt.IsZero() && hostCharge > previousHostCharge {
				rising = true
			}
			previousHostCharge = hostCharge
			prevAt = now

			urgency := ""
			reason := ""
			switch {
			case dOOM > 0:
				urgency = resource.UrgencyHigh
				reason = "oom_event"
			case dHigh > 0:
				urgency = resource.UrgencyNormal
				reason = "high_event"
			case memHigh > 0 && hostCharge > 0 && rising && hostChargeAbove95Percent(hostCharge, memHigh):
				urgency = resource.UrgencyLow
				reason = "predicted"
			}
			if urgency == "" {
				continue
			}
			s.dispatch(urgency, reason)
		}
	}
}

// dispatch never writes CH or cgroup state itself. Pressure signals are
// coalesced by the local controller and remain safe if this producer outruns it.
func (s *PressureSensor) dispatch(urgency, reason string) {
	s.controller.RequestPressureGrow(urgency, reason)
}

func hostChargeAbove95Percent(hostCharge, high uint64) bool {
	if high == 0 {
		return false
	}
	// Compare hostCharge/high > 95/100 without converting byte counts to
	// float64 and without overflowing either product.
	leftHigh, leftLow := bits.Mul64(hostCharge, 100)
	rightHigh, rightLow := bits.Mul64(high, 95)
	return leftHigh > rightHigh || leftHigh == rightHigh && leftLow > rightLow
}

// readMemoryEvents parses cgroup memory.events.local. Returns
// (high, oom, ok). ok=false means the file was not readable.
func readMemoryEvents(cgroupPath string) (high, oom uint64, ok bool) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, "memory.events.local"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "high":
			high = v
		case "oom", "oom_kill":
			if v > oom {
				oom = v
			}
		}
	}
	return high, oom, true
}

// readUintFromCgFile reads a single decimal uint from a cgroup file.
// Returns 0 on read error or "max" content.
func readUintFromCgFile(cgroupPath, name string) uint64 {
	data, err := os.ReadFile(filepath.Join(cgroupPath, name))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// StartSensor spawns the local pressure producer after the launch/restore
// barrier. It applies in static and dynamic cgroup modes and is independent of
// the node reservation session's Settled notification.
func (m *MemoryController) StartSensor(ctx context.Context) {
	if m == nil || m.cgroupPath == "" {
		return
	}
	sensor := NewPressureSensor(m, m.cfg, m.cgroupPath, m.logf)
	go sensor.Run(ctx)
}
