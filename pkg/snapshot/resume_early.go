package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/memory"
	"golang.org/x/sys/unix"
)

// frozenResumeEarlySource is an ephemeral, sealed copy, not a new snapshot format.
// Keep the three-state map: an explicit Zero must never become an inherited Hole.
// RAM Data is still copied while paused: a shared mutable memfd cannot safely
// become an upstream frozen-memory holder while its VM continues writing.
// Ordinary sinks own final persistence; the kernel-copy path adds no new format.
type frozenResumeEarlySource struct {
	*memorySource
	file *os.File
}

func (s *frozenResumeEarlySource) Close() error { return s.file.Close() }

type borrowedResumeEarlySource struct{ sparse.Source }

func (borrowedResumeEarlySource) Close() error { return nil }

func freezeResumeEarlySource(ctx context.Context, input sparse.Source) (*frozenResumeEarlySource, error) {
	return freezeResumeEarlySourceFD(ctx, input, -1)
}

// sourceFD, when supplied, must back input at identical byte offsets. Only RAM
// uses this path; encrypted or layered disk sources always use their reader.
func freezeResumeEarlySourceFD(ctx context.Context, input sparse.Source, sourceFD int) (_ *frozenResumeEarlySource, err error) {
	if input.Size() > uint64(^uint64(0)>>1) {
		return nil, errors.New("resume-early: source is too large")
	}
	fd, err := unix.MemfdCreate("kuasar-resumeEarly", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "kuasar-resumeEarly")
	defer func() {
		if err != nil {
			_ = f.Close()
		}
	}()
	if err = f.Truncate(int64(input.Size())); err != nil {
		return nil, err
	}
	var runs []memoryRun
	var copies []sparse.Run
	for off := uint64(0); off < input.Size(); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, e := input.RunAt(off, input.Size()-off)
		if e != nil {
			return nil, e
		}
		if e = validMemoryRun(run, off, input.Size()); e != nil {
			return nil, e
		}
		end := run.End()
		if len(runs) > 0 && runs[len(runs)-1].kind == run.Kind() {
			runs[len(runs)-1].end = end
		} else {
			runs = append(runs, memoryRun{off: off, end: end, kind: run.Kind()})
		}
		if run.Kind() == sparse.Data {
			for pos := off; pos < end; {
				last := min(end, pos+(1<<20))
				// Capture the original run: sparse.Source implementations need not
				// support concurrent RunAt calls. Its frozen reads are immutable.
				copies = append(copies, memoryRun{off: pos, end: last, kind: sparse.Data, read: func(ctx context.Context, b []byte, offset uint64) (int, error) {
					return run.ReadAt(ctx, b, offset-run.Offset())
				}})
				pos = last
			}
		}
		off = end
	}
	// Two disjoint RAM-copy workers bound resource use without changing the
	// captured bytes or allowing any writer to resume before BOTH have joined.
	workers := 1
	if sourceFD >= 0 && len(copies) > 1 {
		workers = 2
	}
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			var scratch []byte
			fdIn := sourceFD
			for i := w; i < len(copies); i += workers {
				if e := copyResumeEarlyRun(copyCtx, copies[i], f, &fdIn, &scratch); e != nil {
					errs[w] = e
					cancel()
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err = unix.FcntlInt(f.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		return nil, err
	}
	s := &frozenResumeEarlySource{file: f, memorySource: &memorySource{size: input.Size()}}
	s.resolve = func(off, limit uint64) (sparse.Run, error) {
		i := sort.Search(len(runs), func(i int) bool { return runs[i].end > off })
		if i == len(runs) {
			return nil, io.EOF
		}
		r := runs[i]
		r.off = off
		r.end = min(r.end, off+limit)
		r.read = func(ctx context.Context, b []byte, pos uint64) (int, error) {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			return f.ReadAt(b, int64(pos))
		}
		return r, nil
	}
	return s, nil
}

// copyResumeEarlyRun uses explicit offsets; parallel workers never share a file cursor.
func copyResumeEarlyRun(ctx context.Context, run sparse.Run, file *os.File, sourceFD *int, scratch *[]byte) error {
	for pos := run.Offset(); pos < run.End(); {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *sourceFD >= 0 {
			in, out := int64(pos), int64(pos)
			n, err := unix.CopyFileRange(*sourceFD, &in, int(file.Fd()), &out, int(run.End()-pos), 0)
			if n > 0 {
				pos += uint64(n)
			}
			if err == nil {
				if n == 0 {
					return io.ErrUnexpectedEOF
				}
				continue
			}
			if !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) {
				return err
			}
			*sourceFD = -1
			if pos == run.End() {
				continue
			}
		}
		if *scratch == nil {
			*scratch = make([]byte, 1<<20)
		}
		b := (*scratch)[:min(uint64(len(*scratch)), run.End()-pos)]
		n, err := run.ReadAt(ctx, b, pos-run.Offset())
		if err != nil {
			return err
		}
		if n != len(b) {
			return io.ErrUnexpectedEOF
		}
		n, err = file.WriteAt(b, int64(pos))
		if err != nil {
			return err
		}
		if n != len(b) {
			return io.ErrShortWrite
		}
		pos += uint64(n)
	}
	return nil
}

// Called only after guest quiesce, CH pause, backend quiesce and CH snapshot.
// Every live byte source is detached before any writer is resumed. Returned
// views last until all ordinary sink calls finish, including failure paths.
func freezeResumeEarlyInputs(ctx context.Context, s *Sources) (_ func() error, err error) {
	started := time.Now()
	var owned []func() error
	release := func() error {
		var err error
		for i := len(owned) - 1; i >= 0; i-- {
			err = errors.Join(err, owned[i]())
		}
		owned = nil
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, release())
		}
	}()
	s.Diffs = append([]DiskDiff(nil), s.Diffs...)
	for i, d := range s.Diffs {
		if i > 0 && s.PortableConfig.Boot.Disks[i-1].Unbound() {
			continue
		}
		if d.TemporaryCheckpoint != nil && d.OpenCheckpoint != nil && d.DeleteCheckpoint != nil {
			var nonce [16]byte
			if _, e := rand.Read(nonce[:]); e != nil {
				return nil, e
			}
			id := "resume-early-" + hex.EncodeToString(nonce[:])
			// Creation can commit before reporting an I/O error. Register cleanup first;
			// the durable temporary flag also covers process death and failed cleanup.
			owned = append(owned, func() error { return d.DeleteCheckpoint(context.Background(), id) })
			if e := d.TemporaryCheckpoint(ctx, id); e != nil {
				return nil, fmt.Errorf("resume-early checkpoint disk %d: %w", i, e)
			}
			// The retained checkpoint already forces CoW. Open its reader only
			// when the sink runs, after guest resume; the sink owns that reader.
			// Capture admission excludes deletion until release below.
			s.Diffs[i].Export = func(ctx context.Context) (ExportView, error) { return d.OpenCheckpoint(ctx, id) }
			continue
		}
		// Copy only this disk's upper layer. The existing sink still handles its
		// immutable lower references and normal merge policy after resumption.
		active := d
		active.MergeBase = ""
		source, closeSource, e := prepareDiskSource(ctx, active, s.LocalCodec, s.LocalRequired, s.MergeBaseOpener)
		if e != nil {
			return nil, fmt.Errorf("resume-early disk %d: %w", i, e)
		}
		frozen, e := freezeResumeEarlySource(ctx, source)
		closeErr := closeSource()
		if frozen != nil {
			owned = append(owned, frozen.Close)
		}
		if e != nil || closeErr != nil {
			return nil, errors.Join(e, closeErr)
		}
		s.Diffs[i].Export = func(context.Context) (ExportView, error) { return borrowedResumeEarlySource{frozen}, nil }
	}
	if s.Logf != nil {
		s.Logf("resume-early freeze: disks_ms=%.3f", float64(time.Since(started))/float64(time.Millisecond))
	}
	memoryStarted := time.Now()
	var source sparse.Source
	if s.CaptureMemory != nil {
		view, e := s.CaptureMemory(ctx)
		if e != nil {
			return nil, e
		}
		source, _, e = CaptureMemory(fdReaderAt(s.MemfdFD), view, true)
		if e != nil {
			return nil, e
		}
		// CaptureView is an immutable bitmap snapshot; the new RAM reader replaces
		// its only live dependency. C/W and released-page semantics are unchanged.
		s.CaptureMemory = func(context.Context) (*memory.CaptureView, error) { return view, nil }
	} else {
		var holes, zeros []sparse.Extent
		if s.MemoryLayout != nil {
			holes, zeros, err = s.MemoryLayout()
		} else {
			holes, err = WalkHoles(s.MemfdFD, s.MemfdSize)
		}
		if err != nil {
			return nil, err
		}
		source = &releasedMemorySource{fd: s.MemfdFD, size: uint64(s.MemfdSize), holes: holes, zeros: zeros}
		s.MemoryLayout = func() ([]sparse.Extent, []sparse.Extent, error) { return holes, zeros, nil }
	}
	frozen, e := freezeResumeEarlySourceFD(ctx, source, s.MemfdFD)
	if e != nil {
		return nil, e
	}
	owned = append(owned, frozen.Close)
	s.MemfdFD = int(frozen.file.Fd())
	if s.Logf != nil {
		s.Logf("resume-early freeze: memory_ms=%.3f", float64(time.Since(memoryStarted))/float64(time.Millisecond))
	}
	return release, nil
}
