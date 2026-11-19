package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/resource"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

type checkpointMemorySave struct {
	holder    *memorycheckpoint.Holder
	hooks     *resctl.ControllerHooks
	retention resource.CaptureRetention
	dump      func(context.Context, io.ReaderAt) (string, error)
	pin       *os.File
	logf      func(string, ...any)
}

var errMemorySaveAbortPending = errors.New("checkpoint memory: rollback acknowledgement is pending")

const memoryRetirementResponseTimeout = 5 * time.Second

func prepareCheckpointMemorySave(ctx context.Context, req ctl.Request, opts RunOptions, mfd *memory.Memfd, runDir string) (*checkpointMemorySave, error) {
	if !opts.EnableCheckpoint || req.CheckpointID == "" || req.ResumeAfter || opts.memoryAsyncDisabled || opts.runtime == nil || req.Mode == ctl.SnapshotModeBundle {
		return nil, nil
	}
	if opts.CurrentRun != nil {
		if req.HostRetirement == nil {
			return nil, nil
		} // older host: preserve synchronous capture
		p := req.HostRetirement
		if err := p.Validate(); err != nil {
			return nil, err
		}
		if p.SandboxID != opts.CurrentRun.SandboxID || p.InstanceID != opts.CurrentRun.InstanceID || p.RunID != opts.CurrentRun.RunID || p.CheckpointID != req.CheckpointID {
			return nil, errors.New("snapshot: host retirement permit identity mismatch")
		}
	} else if req.HostRetirement != nil {
		return nil, errors.New("snapshot: managed permit on standalone runtime")
	}
	hooks := opts.runtime.hooks
	if hooks != nil && hooks.Enabled() && !hooks.SupportsRetainedMemory() {
		return nil, nil
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	memoryID := hex.EncodeToString(id[:])
	start, err := memorycheckpoint.ProcessStartTime(os.Getpid())
	if err != nil {
		return nil, err
	}
	r := memorycheckpoint.CaptureRecord{Version: memorycheckpoint.Version, Producer: memorycheckpoint.RunIdentity{SandboxID: opts.SandboxID}, CheckpointID: req.CheckpointID, MemoryID: memoryID, Epoch: 1, Holder: memorycheckpoint.HolderIdentity{PID: os.Getpid(), StartTime: start, SocketPath: filepath.Join(runDir, "memory-holders", memoryID, "1", "m.sock")}}
	if opts.CurrentRun != nil {
		r.Producer.InstanceID = opts.CurrentRun.InstanceID
		r.Producer.RunID = opts.CurrentRun.RunID
	}
	if req.HostRetirement != nil {
		r.HostOperationID = req.HostRetirement.OperationID
	}
	pin, err := memorycheckpoint.PinDirectory(opts.checkpointDir)
	if err != nil {
		return nil, err
	}
	record, err := memorycheckpoint.CreateRecord(opts.checkpointDir, r)
	if err != nil {
		pin.Close()
		return nil, err
	}
	fd, err := memorycheckpoint.ReadOnlyMemfd(mfd.FD())
	if err != nil {
		return nil, errors.Join(err, memorycheckpoint.AbortRecord(record, pin.Close, opts.memoryLogf))
	}
	holder, err := memorycheckpoint.NewHolder(record, fd, uint64(mfd.Size()), opts.memoryLogf)
	if err != nil {
		closeErr := fd.Close()
		return nil, errors.Join(err, closeErr, memorycheckpoint.AbortRecord(record, pin.Close, opts.memoryLogf))
	}
	op := r.HostOperationID
	if op == "" {
		op = r.CheckpointID
	}
	save := &checkpointMemorySave{holder: holder, hooks: hooks, pin: pin, logf: opts.memoryLogf, retention: resource.CaptureRetention{CapturePath: record.Path(), MemoryID: r.MemoryID, Epoch: r.Epoch, OperationID: op}}
	if hooks != nil && hooks.Enabled() {
		if _, err := hooks.PrepareRetained(ctx, save.retention); err != nil {
			return nil, errors.Join(err, save.abort())
		}
	}
	return save, nil
}

func (s *checkpointMemorySave) abort() error {
	if s == nil || s.holder.Record().Read().Committed {
		return nil
	}
	err := s.holder.AbortWithCleanup(s.pin.Close)
	if s.hooks != nil && s.hooks.Enabled() {
		err = errors.Join(err, s.hooks.AbortRetained(context.Background(), s.retention))
	}
	if err != nil {
		return errors.Join(errMemorySaveAbortPending, err)
	}
	return nil
}

func (s *checkpointMemorySave) capture(opts RunOptions, parents []string, working bool) *snapshot.FrozenMemoryCapture {
	return &snapshot.FrozenMemoryCapture{
		Build: func(ctx context.Context, sink snapshot.ArtifactSink, configJSON, stateJSON, snapshotConfig []byte, view *memory.CaptureView) (sparse.Source, uint64, error) {
			// The source is used for metadata validation only; the asynchronous
			// writer receives its independently owned read-only descriptor later.
			top, _, err := snapshot.CaptureMemory(zeroReader{}, view, working)
			if err != nil {
				return nil, 0, err
			}
			if err = snapshot.ValidateMemoryPayload(ctx, top); err != nil {
				return nil, 0, err
			}
			encoded, err := memorycheckpoint.EncodeMap(view)
			if err != nil {
				return nil, 0, err
			}
			mapSource, err := sparse.NewSource(bytes.NewReader(encoded), uint64(len(encoded)), nil)
			if err != nil {
				return nil, 0, err
			}
			mapRef, _, err := sink.AbsorbImageSource(ctx, mapSource)
			if err != nil {
				return nil, 0, err
			}
			r := s.holder.Record().Read()
			d := memorycheckpoint.Descriptor{Version: memorycheckpoint.Version, MemoryID: r.MemoryID, CheckpointID: r.CheckpointID, Size: view.Size, MapRef: mapRef, Parents: append([]string(nil), parents...), WorkingSet: working}
			source, err := snapshotfile.BuildLocalSource(configJSON, stateJSON, snapshotConfig, d)
			if err != nil {
				return nil, 0, err
			}
			// Do not retain opts, Runtime, ProcessStorage or its borrowed fetcher.
			directory, sid, codec, required, logf := opts.checkpointDir, opts.SandboxID, opts.LocalCodec, opts.LocalRequired, opts.memoryLogf
			s.dump = func(ctx context.Context, reader io.ReaderAt) (string, error) {
				payload, _, err := snapshot.CaptureMemory(reader, view, working)
				if err != nil {
					return "", err
				}
				sink := snapshot.NewFileSink(directory, sid, codec, required, logf)
				ref, _, err := sink.AbsorbImageSource(ctx, payload)
				return ref, errors.Join(err, sink.Close())
			}
			var resident uint64
			for off := uint64(0); off < top.Size(); {
				run, err := top.RunAt(off, top.Size()-off)
				if err != nil {
					return nil, 0, err
				}
				if run.Kind() != sparse.Hole {
					resident += run.End() - off
				}
				off = run.End()
			}
			return source, resident, nil
		},
		Commit: func(out *snapshot.Outputs) error {
			err := s.holder.Record().Update(func(r *memorycheckpoint.CaptureRecord) {
				r.SnapshotRef = out.SnapshotRef
				r.SandboxRef = out.SandboxRef
				r.Committed = true
			})
			if !s.holder.Record().Read().Committed {
				return err
			}
			opts.runtime.mu.Lock()
			opts.runtime.memorySave = s
			opts.runtime.mu.Unlock()
			if err != nil {
				return errors.Join(snapshot.ErrFrozenMemoryCommitted, err)
			}
			return nil
		},
	}
}

type zeroReader struct{}

func (zeroReader) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("metadata-only memory reader was used for payload")
}

func (s *checkpointMemorySave) retire() error {
	if s == nil {
		return nil
	}
	// Cleanup has already retired the VMM. Keep the remaining persistence
	// and account handoff alive independently, including after an errored
	// response, without retaining the caller's operation/source locks forever.
	first := make(chan error, 1)
	go s.finishRetirement(first)
	timer := time.NewTimer(memoryRetirementResponseTimeout)
	defer timer.Stop()
	select {
	case err := <-first:
		return err
	case <-timer.C:
		return errors.New("checkpoint memory: runtime cleanup completed; retirement publication is still pending")
	}
}

func (s *checkpointMemorySave) finishRetirement(first chan<- error) {
	firstResult := true
	report := func(err error) {
		if firstResult {
			first <- err
			firstResult = false
		}
		if err != nil && s.logf != nil {
			s.logf("checkpoint memory retirement retained for retry: %v", err)
		}
	}
	for {
		if err := s.holder.Record().Update(func(r *memorycheckpoint.CaptureRecord) { r.RuntimeRetired = true }); err == nil {
			break
		} else {
			report(fmt.Errorf("checkpoint memory: publish retired runtime: %w", err))
		}
		time.Sleep(time.Second)
	}
	// This exact source account is retained even when the ordinary resource
	// Release defer ran first. Commit does not send a zero budget to the VM.
	if s.hooks != nil && s.hooks.Enabled() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := s.hooks.CommitRetained(ctx, s.retention)
			cancel()
			if err == nil {
				break
			}
			report(fmt.Errorf("checkpoint memory: commit retained account: %w", err))
			time.Sleep(time.Second)
		}
	}
	err := s.holder.Start(s.dump, func(ctx context.Context) error {
		if s.hooks != nil && s.hooks.Enabled() {
			if err := s.hooks.ReleaseRetained(ctx, s.retention); err != nil {
				return err
			}
			if err := s.hooks.CloseRetained(); err != nil {
				return err
			}
		}
		return s.pin.Close()
	})
	report(err)
}

// WaitMemorySave keeps executable adapters alive for their detached holder.
// Runtime.Wait/Close retain their existing resource-ownership boundary.
func (r *Runtime) WaitMemorySave(ctx context.Context) error {
	r.mu.Lock()
	save := r.memorySave
	r.mu.Unlock()
	if save == nil {
		return nil
	}
	return save.holder.Wait(ctx)
}

// HasRuntimeOwner lets a legacy executable adapter join the same lifecycle
// without nesting a second owner around an existing RunLifecycle call.
func HasRuntimeOwner(ctx context.Context) bool {
	_, ok := ctx.Value(runtimeOwnerKey{}).(*Runtime)
	return ok
}

// PreviousCaptureRetention is selected by SID directory rather than the
// chosen restore root. A completed record is also supplied: its resource ACK
// or a previously prepared owner handoff may still need idempotent replay.
func PreviousCaptureRetention(directory, sid string) (*resource.CaptureRetention, error) {
	r, err := memorycheckpoint.ReadCurrent(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Producer.SandboxID != sid {
		return nil, errors.New("checkpoint memory: retained sandbox identity mismatch")
	}
	if r.Aborted {
		return nil, nil
	}
	if !r.Committed || !r.RuntimeRetired {
		return nil, errors.New("checkpoint memory: previous runtime has not retired")
	}
	path, err := memorycheckpoint.RecordPath(directory, r.CheckpointID)
	if err != nil {
		return nil, err
	}
	op := r.HostOperationID
	if op == "" {
		op = r.CheckpointID
	}
	return &resource.CaptureRetention{CapturePath: path, MemoryID: r.MemoryID, Epoch: r.Epoch, OperationID: op}, nil
}

func destroyFrozenMemory(process processSignaler, lock *os.File, release func(), exited <-chan struct{}, logf func(string, ...any)) {
	// CH shutdown can run device work. A committed frozen backing permits no
	// further writer, so retire the already-paused VMM without resuming it.
	defer func() {
		if lock != nil {
			_ = lock.Close()
		}
		release()
	}()
	if process == nil || exited == nil {
		logf("checkpoint memory: missing frozen VMM owner")
		return
	}
	if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		logf("checkpoint memory: retire VMM: %v", err)
	}
	<-exited
}

func frozenRetirement(r *Runtime, process processSignaler, lock *os.File, release func(), exited <-chan struct{}, logf func(string, ...any)) func() error {
	return sync.OnceValue(func() error {
		go destroyFrozenMemory(process, lock, release, exited, logf)
		<-r.exitDone
		return r.exit.Err
	})
}

// previousMemorySave belongs to the new Runtime, including when it restores an
// older checkpoint. Its only transition is pending -> ready.
type previousMemorySave struct {
	mu        sync.Mutex
	record    *memorycheckpoint.CaptureRecord
	directory string
	ready     bool
}

func newPreviousMemorySave(directory, sid string) (*previousMemorySave, error) {
	p := &previousMemorySave{directory: directory, ready: true}
	r, err := memorycheckpoint.ReadCurrent(directory)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Producer.SandboxID != sid {
		return nil, errors.New("checkpoint memory: previous sandbox identity mismatch")
	}
	if !r.RAMReleased && !r.Aborted {
		p.record = r
		p.ready = false
	}
	return p, nil
}

func (p *previousMemorySave) wait(ctx context.Context, noWait bool) (*memorycheckpoint.CaptureRecord, error) {
	if p == nil {
		return nil, nil
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		if p.ready {
			r := p.record
			p.mu.Unlock()
			return r, nil
		}
		id := *p.record
		p.mu.Unlock()
		r, err := memorycheckpoint.ReadRecord(p.directory, id.CheckpointID)
		if err != nil {
			return nil, err
		}
		if r.MemoryID != id.MemoryID || r.Epoch != id.Epoch {
			return nil, errors.New("checkpoint memory: previous save identity changed")
		}
		if r.RAMReleased || r.Aborted {
			p.mu.Lock()
			p.ready = true
			p.record = r
			p.mu.Unlock()
			return r, nil
		}
		if noWait {
			return r, ctl.ErrMemorySavePending
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (h *SnapshotHandler) captureState(ctx context.Context, q ctl.CaptureStateQuery) (ctl.CaptureState, error) {
	state := ctl.CaptureState{SandboxID: h.SandboxID}
	if h.CurrentRun != nil {
		state.InstanceID = h.CurrentRun.InstanceID
		state.RunID = h.CurrentRun.RunID
	}
	if q.SandboxID != state.SandboxID || q.InstanceID != state.InstanceID || q.RunID != state.RunID {
		return state, errors.New("capture state requested for another runtime")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	life := h.Context
	if life == nil {
		life = context.Background()
	}
	stop := context.AfterFunc(life, cancel)
	defer stop()
	r, err := h.previousMemory.wait(ctx, q.NoWaitForPreviousMemorySave)
	if r != nil {
		state.MemoryID = r.MemoryID
		state.Epoch = r.Epoch
	}
	if errors.Is(err, ctl.ErrMemorySavePending) {
		state.Pending = true
		return state, nil
	}
	return state, err
}

func checkpointMemoryParents(ctx context.Context, opts RunOptions) (parents, external []string, err error) {
	if opts.MemoryBinding == nil {
		return nil, nil, nil
	}
	b := opts.MemoryBinding
	all := prependRef(b.SnapshotRef, b.FromRefs)
	parents, err = checkpointPrefix(ctx, all, b.RuntimeRef, b.RelativeDir, opts, true)
	if err != nil {
		return nil, nil, err
	}
	if len(parents) == 0 {
		external, err = memoryRefsForSnapshot(b)
	} else {
		external = append([]string(nil), all[len(parents):]...)
	}
	return parents, external, err
}
