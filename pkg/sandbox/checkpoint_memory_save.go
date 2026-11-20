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
	parents   []string
	external  []string
	dump      func(context.Context, io.ReaderAt) (string, error)
	newSink   func() (snapshot.ArtifactSink, error)
	logf      func(string, ...any)
}

var errMemorySaveAbortPending = errors.New("checkpoint memory: rollback acknowledgement is pending")

const memoryRetirementResponseTimeout = 5 * time.Second

// checkpointMemoryRetention describes whether this attempt can create a new
// frozen-RAM obligation. Admission and preparation share it; file encoding does
// not select a different lifecycle.
func checkpointMemoryRetention(req ctl.Request, opts RunOptions) (bool, error) {
	if !checkpointMemoryRetentionSupported(opts) || req.CheckpointID == "" || req.ResumeAfter {
		return false, nil
	}
	if _, err := req.SnapshotMode(); err != nil {
		return false, err
	}
	if opts.CurrentRun != nil {
		if req.HostRetirement == nil {
			return false, nil
		} // older host: preserve synchronous capture
		p := req.HostRetirement
		if err := p.Validate(); err != nil {
			return false, err
		}
		if p.SandboxID != opts.CurrentRun.SandboxID || p.InstanceID != opts.CurrentRun.InstanceID || p.RunID != opts.CurrentRun.RunID || p.CheckpointID != req.CheckpointID {
			return false, errors.New("snapshot: host retirement permit identity mismatch")
		}
	} else if req.HostRetirement != nil {
		return false, errors.New("snapshot: managed permit on standalone runtime")
	}
	return true, nil
}

func checkpointMemoryRetentionSupported(opts RunOptions) bool {
	if !opts.EnableCheckpoint || opts.memoryAsyncDisabled || opts.runtime == nil {
		return false
	}
	hooks := opts.runtime.hooks
	return hooks == nil || !hooks.Enabled() || hooks.SupportsRetainedMemory()
}

func prepareCheckpointMemorySave(ctx context.Context, req ctl.Request, opts RunOptions, mfd *memory.Memfd, runDir string) (*checkpointMemorySave, error) {
	retain, err := checkpointMemoryRetention(req, opts)
	if err != nil || !retain {
		return nil, err
	}
	hooks := opts.runtime.hooks
	parents, external, err := checkpointMemoryParents(ctx, opts, !req.MergeRefEnabled())
	if err != nil {
		return nil, fmt.Errorf("snapshot: memory history: %w", err)
	}
	newSink, err := checkpointMemorySink(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	memoryID := hex.EncodeToString(id[:])
	r := memorycheckpoint.CaptureRecord{Version: memorycheckpoint.Version, Producer: memorycheckpoint.RunIdentity{SandboxID: opts.SandboxID}, CheckpointID: req.CheckpointID, MemoryID: memoryID, Epoch: 1}
	if opts.CurrentRun != nil {
		r.Producer.InstanceID = opts.CurrentRun.InstanceID
		r.Producer.RunID = opts.CurrentRun.RunID
	}
	if req.HostRetirement != nil {
		r.HostOperationID = req.HostRetirement.OperationID
	}
	fd, err := memorycheckpoint.ReadOnlyMemfd(mfd.FD())
	if err != nil {
		return nil, err
	}
	holder, err := memorycheckpoint.PrepareHolder(opts.checkpointDir, runDir, r, fd, uint64(mfd.Size()), opts.memoryLogf)
	if err != nil {
		return nil, err
	}
	record := holder.Record()
	r = record.Read()
	op := r.HostOperationID
	if op == "" {
		op = r.CheckpointID
	}
	save := &checkpointMemorySave{holder: holder, hooks: hooks, parents: parents, external: external, newSink: newSink, logf: opts.memoryLogf, retention: resource.CaptureRetention{CapturePath: record.Path(), MemoryID: r.MemoryID, Epoch: r.Epoch, OperationID: op}}
	if hooks != nil && hooks.Enabled() {
		if _, err := hooks.PrepareRetained(ctx, save.retention); err != nil {
			return nil, errors.Join(err, save.abort())
		}
	}
	return save, nil
}

// checkpointMemorySink fixes the encoding inputs before capture. The detached
// writer retains only immutable configuration and the resolved customer key,
// never the Runtime, ProcessStorage or a borrowed key-resolver closure.
func checkpointMemorySink(ctx context.Context, req ctl.Request, opts RunOptions) (func() (snapshot.ArtifactSink, error), error) {
	directory, sid, codec, required, logf := opts.checkpointDir, opts.SandboxID, opts.LocalCodec, opts.LocalRequired, opts.memoryLogf
	if req.Mode != ctl.SnapshotModeBundle {
		return func() (snapshot.ArtifactSink, error) {
			return snapshot.NewFileSink(directory, sid, codec, required, logf), nil
		}, nil
	}
	if opts.ManifestCfg == nil || opts.CustomerKeyFn == nil {
		return nil, errors.New("checkpoint Bundle requires manifest configuration and customer key resolver")
	}
	cfg := *opts.ManifestCfg
	key, err := opts.CustomerKeyFn()
	if err != nil {
		return nil, fmt.Errorf("checkpoint Bundle customer key: %w", err)
	}
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		return nil, fmt.Errorf("checkpoint Bundle admission: %w", err)
	}
	newSink := func() (snapshot.ArtifactSink, error) {
		return snapshot.NewCheckpointBundleSink(directory, sid, &cfg, func() ([32]byte, error) { return key, nil }, admission, logf)
	}
	probe, err := newSink()
	if err != nil {
		return nil, err
	}
	if err := probe.Close(); err != nil {
		return nil, err
	}
	return newSink, nil
}

func (s *checkpointMemorySave) abort() error {
	if s == nil || s.holder.Record().Read().Committed {
		return nil
	}
	err := s.holder.Abort()
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
			// CaptureMemory has validated the fixed page geometry. The extra
			// GNU sparse extent/index limits apply only to the tarstream writer,
			// just as in the existing synchronous BuildCaptureSource path.
			if _, file := sink.(*snapshot.FileSink); file {
				if err = snapshot.ValidateMemoryPayload(ctx, top); err != nil {
					return nil, 0, err
				}
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
			newSink := s.newSink
			s.dump = func(ctx context.Context, reader io.ReaderAt) (string, error) {
				payload, _, err := snapshot.CaptureMemory(reader, view, working)
				if err != nil {
					return "", err
				}
				sink, err := newSink()
				if err != nil {
					return "", err
				}
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
		return nil
	})
	report(err)
}

// WaitMemorySave keeps executable adapters alive for their detached holder and
// final capture responses. Runtime.Wait/Close retain their existing resource-
// ownership boundary; response completion must not join that earlier barrier.
func (r *Runtime) WaitMemorySave(ctx context.Context) error {
	r.mu.Lock()
	save := r.memorySave
	responsesDone := r.captureResponsesDone
	r.mu.Unlock()
	if save == nil {
		return nil
	}
	err := save.holder.Wait(ctx)
	if responsesDone != nil {
		select {
		case <-responsesDone:
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		}
	}
	return err
}

// HasRuntimeOwner lets a legacy executable adapter join the same lifecycle
// without nesting a second owner around an existing RunLifecycle call.
func HasRuntimeOwner(ctx context.Context) bool {
	_, ok := ctx.Value(runtimeOwnerKey{}).(*Runtime)
	return ok
}

// PreviousCaptureRetention scans every capture for a local inventory hint.
// The node independently resolves all account entries; this does not select a
// billing subset or depend on the requested restore root/current-memory hint.
func PreviousCaptureRetention(directory, sid string) (*resource.CaptureRetention, error) {
	records, err := memorycheckpoint.ListRecords(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var hint *resource.CaptureRetention
	for _, r := range records {
		if r.Producer.SandboxID != sid {
			return nil, errors.New("checkpoint memory: retained sandbox identity mismatch")
		}
		if r.Aborted {
			continue
		}
		if !r.Committed || !r.RuntimeRetired {
			return nil, errors.New("checkpoint memory: previous runtime has not retired")
		}
		path, err := memorycheckpoint.RecordPath(directory, r.CheckpointID, r.MemoryID)
		if err != nil {
			return nil, err
		}
		op := r.HostOperationID
		if op == "" {
			op = r.CheckpointID
		}
		if hint == nil {
			hint = &resource.CaptureRetention{CapturePath: path, MemoryID: r.MemoryID, Epoch: r.Epoch, OperationID: op}
		}
	}
	return hint, nil
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

// Capture admission depends on this run's capability, never on an older
// holder's save progress. The diagnostic query still reports all generations.
func (h *SnapshotHandler) captureState(ctx context.Context, q ctl.CaptureStateQuery) (ctl.CaptureState, error) {
	state := ctl.CaptureState{LifecycleVersion: ctl.CheckpointLifecycleVersion, SandboxID: h.SandboxID}
	if h.CurrentRun != nil {
		state.InstanceID, state.RunID = h.CurrentRun.InstanceID, h.CurrentRun.RunID
	}
	if q.SandboxID != state.SandboxID || q.InstanceID != state.InstanceID || q.RunID != state.RunID {
		return state, errors.New("capture state requested for another runtime")
	}
	if err := ctx.Err(); err != nil {
		return state, err
	}
	if q.ForCapture {
		supported := checkpointMemoryRetentionSupported(RunOptions{EnableCheckpoint: h.EnableCheckpoint, runtime: h.runtime})
		state.RetentionSupported = &supported
		return state, nil
	}
	if h.CheckpointDir == "" {
		return state, nil
	}
	records, err := memorycheckpoint.ListRecords(h.CheckpointDir)
	if err != nil {
		return state, err
	}
	for _, record := range records {
		if record.Producer.SandboxID != h.SandboxID {
			return state, errors.New("capture state producer mismatch")
		}
		if !record.Aborted && !record.RAMReleased {
			state.Pending = true
			state.MemoryID, state.Epoch = record.MemoryID, record.Epoch
			break
		}
	}
	return state, nil
}

func checkpointMemoryParents(ctx context.Context, opts RunOptions, working bool) (parents, external []string, err error) {
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
	if err != nil {
		return nil, nil, err
	}
	// A portable working-set root preserves this round's W above one dirty H.
	// Reserve that H slot before preparing a holder or retaining resources;
	// internal local parents must not hide an eventual from_refs overflow.
	if working && len(parents) != 0 && len(external) >= snapshot.MaxMemoryFromRefs {
		return nil, nil, fmt.Errorf("snapshot from_refs exceeds %d entries: working-set history requires one entry", snapshot.MaxMemoryFromRefs)
	}
	if err := validateProspectiveMemoryConfig(external); err != nil {
		return nil, nil, err
	}
	return parents, external, nil
}
