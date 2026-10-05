package vhost

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"syscall"
)

// BlockDeviceSpec fixes the Guest-visible shape of a vhost block device.
// Devices use the existing minimal virtio feature profile; slice order fixes
// device order. Neither capacity nor read-only capability changes at binding.
type BlockDeviceSpec struct {
	Capacity int64 // bytes, positive and a multiple of SectorSize
	ReadOnly bool
}

// ErrBackendUnbound is a normal device I/O failure, not a fatal storage error
// or a request to retry. Boot-time probes must complete even before Launch.
var ErrBackendUnbound = fmt.Errorf("vhost: workload backend is unbound: %w", syscall.EIO)

// ErrBackendSetBound reports an attempt to replace a committed workload.
var ErrBackendSetBound = errors.New("vhost: workload backends already bound")

type backendBinding struct {
	backends []Backend
}

// BackendSet separates fixed devices from a single workload's storage. Bind
// validates and publishes the entire set at one atomic publication point.
// The runtime, not this set, owns and closes supplied backends after requests
// have drained. A failed Bind never transfers ownership or closes an input.
// BackendSet must not be copied after construction.
type BackendSet struct {
	specs   []BlockDeviceSpec
	devices []Backend
	binding atomic.Pointer[backendBinding]
}

func NewBackendSet(specs []BlockDeviceSpec) (*BackendSet, error) {
	s := &BackendSet{specs: append([]BlockDeviceSpec(nil), specs...)}
	s.devices = make([]Backend, len(specs))
	for i, spec := range s.specs {
		if spec.Capacity <= 0 || spec.Capacity%SectorSize != 0 {
			return nil, fmt.Errorf("vhost: device %d capacity %d must be positive and sector-aligned", i, spec.Capacity)
		}
		s.devices[i] = &backendSlot{set: s, index: i, unbound: unboundBackend{spec: spec}}
	}
	return s, nil
}

// DeviceBackends returns stable frontends suitable for NewServer. The returned
// slice is independent of the set. Each frontend retains its original shape.
func (s *BackendSet) DeviceBackends() []Backend {
	return append([]Backend(nil), s.devices...)
}

// Bind may succeed only once. A rejected candidate leaves every device
// unbound; concurrent candidates cannot install a mixture of their backends.
// Inputs must be prepared storage backends, not DeviceBackends frontends: a
// frontend-to-frontend binding could remain unbound or form a recursive cycle.
// Callers must not mutate the slice while Bind is copying it, or mutate a
// backend's shape for the remainder of its lifetime.
func (s *BackendSet) Bind(backends []Backend) error {
	if s.binding.Load() != nil {
		return ErrBackendSetBound
	}
	if len(backends) != len(s.specs) {
		return fmt.Errorf("vhost: backend count %d does not match device count %d", len(backends), len(s.specs))
	}
	next := &backendBinding{backends: append([]Backend(nil), backends...)}
	for i, b := range next.backends {
		if nilBackend(b) {
			return fmt.Errorf("vhost: device %d backend is nil", i)
		}
		if _, frontend := b.(*backendSlot); frontend {
			return fmt.Errorf("vhost: device %d cannot bind another device frontend", i)
		}
		spec := s.specs[i]
		if size := b.Size(); size != spec.Capacity {
			return fmt.Errorf("vhost: device %d backend size %d does not match capacity %d", i, size, spec.Capacity)
		}
		if b.ReadOnly() != spec.ReadOnly {
			return fmt.Errorf("vhost: device %d backend read-only capability does not match device", i)
		}
	}
	if !s.binding.CompareAndSwap(nil, next) {
		return ErrBackendSetBound
	}
	return nil
}

// Detect typed nils before invoking methods on caller-provided backends. This
// is construction-time validation, never part of the block I/O hot path.
func nilBackend(b Backend) bool {
	if b == nil {
		return true
	}
	v := reflect.ValueOf(b)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

type unboundBackend struct{ spec BlockDeviceSpec }

func (*unboundBackend) suppressRequestStats() bool         { return true }
func (b *unboundBackend) Size() int64                      { return b.spec.Capacity }
func (b *unboundBackend) ReadOnly() bool                   { return b.spec.ReadOnly }
func (*unboundBackend) ReadAt([]byte, int64) (int, error)  { return 0, ErrBackendUnbound }
func (*unboundBackend) WriteAt([]byte, int64) (int, error) { return 0, ErrBackendUnbound }
func (*unboundBackend) Flush() error                       { return ErrBackendUnbound }
func (*unboundBackend) Discard(int64, int64) error         { return ErrBackendUnbound }

type backendSlot struct {
	set     *BackendSet
	index   int
	unbound unboundBackend
}

func (b *backendSlot) Size() int64    { return b.unbound.Size() }
func (b *backendSlot) ReadOnly() bool { return b.unbound.ReadOnly() }

// requestBackend pins one immutable binding view for an entire virtio request,
// not one view per data segment. A request selected before commit may finish
// with IOERR; a request selected after commit sees the real backend throughout.
func (b *backendSlot) requestBackend() Backend {
	if binding := b.set.binding.Load(); binding != nil {
		return binding.backends[b.index]
	}
	return &b.unbound
}

func (b *backendSlot) ReadAt(p []byte, off int64) (int, error) {
	return b.requestBackend().ReadAt(p, off)
}
func (b *backendSlot) WriteAt(p []byte, off int64) (int, error) {
	return b.requestBackend().WriteAt(p, off)
}
func (b *backendSlot) Flush() error               { return b.requestBackend().Flush() }
func (b *backendSlot) Discard(off, n int64) error { return b.requestBackend().Discard(off, n) }
func (b *backendSlot) readAt(ctx context.Context, p []byte, off int64) (int, error) {
	return readBackendAt(ctx, b.requestBackend(), p, off)
}
func (b *backendSlot) writeAt(ctx context.Context, p []byte, off int64) (int, error) {
	return writeBackendAt(ctx, b.requestBackend(), p, off)
}
func (b *backendSlot) BackendStats() map[string]any {
	if reporter, ok := b.requestBackend().(StatsReporter); ok {
		return reporter.BackendStats()
	}
	return nil
}

func backendForRequest(b Backend) Backend {
	if selector, ok := b.(interface{ requestBackend() Backend }); ok {
		return selector.requestBackend()
	}
	return b
}
