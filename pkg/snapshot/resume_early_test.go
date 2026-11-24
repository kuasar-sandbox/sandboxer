package snapshot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"golang.org/x/sys/unix"
)

func TestResumeEarlyFrozenSourcePreservesStatesAndDetachesWrites(t *testing.T) {
	live := bytes.Repeat([]byte{0x71}, 3*4096)
	src := &memorySource{size: uint64(len(live))}
	src.resolve = func(off, limit uint64) (sparse.Run, error) {
		kind := []sparse.RunKind{sparse.Data, sparse.Zero, sparse.Hole}[off/4096]
		return memoryRun{off: off, end: min((off/4096+1)*4096, off+limit), kind: kind, read: func(_ context.Context, b []byte, p uint64) (int, error) { return copy(b, live[p:]), nil }}, nil
	}
	frozen, err := freezeResumeEarlySource(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Close()
	clear(live)
	for i, kind := range []sparse.RunKind{sparse.Data, sparse.Zero, sparse.Hole} {
		r, err := frozen.RunAt(uint64(i*4096), 4096)
		if err != nil || r.Kind() != kind {
			t.Fatal(i, r, err)
		}
		b := make([]byte, 4096)
		if n, err := frozen.ReadAt(context.Background(), b, uint64(i*4096)); n != 4096 || err != nil {
			t.Fatal(n, err)
		}
		want := byte(0)
		if i == 0 {
			want = 0x71
		}
		if !bytes.Equal(b, bytes.Repeat([]byte{want}, 4096)) {
			t.Fatal("live writer changed frozen payload", i)
		}
	}
	if _, err := frozen.file.WriteAt([]byte{1}, 0); !errors.Is(err, unix.EPERM) {
		t.Fatal("frozen file is writable", err)
	}
}

func TestResumeEarlyCanceledCopyReleasesTemporaryFile(t *testing.T) {
	count := func() int {
		rows, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(rows)
	}
	before := count()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source, _ := sparse.NewSource(bytes.NewReader(make([]byte, 4096)), 4096, nil)
	if _, err := freezeResumeEarlySource(ctx, source); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if after := count(); after != before {
		t.Fatalf("FD leaked: %d -> %d", before, after)
	}
}

func TestResumeEarlyShortReadCannotPublishFrozenInput(t *testing.T) {
	source, _ := sparse.NewSource(bytes.NewReader([]byte{1}), 4096, nil)
	if frozen, err := freezeResumeEarlySource(context.Background(), source); err == nil || frozen != nil {
		t.Fatal(err)
	}
}

func TestResumeEarlyKernelCopyKeepsSparseFrozenRAM(t *testing.T) {
	fd, err := unix.MemfdCreate("resume-early-input", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	live := os.NewFile(uintptr(fd), "resume-early-input")
	defer live.Close()
	const size = 4 << 20
	if err := live.Truncate(size); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0x72}, 2<<20)
	if _, err := live.WriteAt(payload, 0); err != nil {
		t.Fatal(err)
	}
	src := &releasedMemorySource{fd: fd, size: size, holes: []sparse.Extent{{Offset: 2 << 20, Size: 2 << 20}}}
	frozen, err := freezeResumeEarlySourceFD(context.Background(), src, fd)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Close()
	if _, err := live.WriteAt(bytes.Repeat([]byte{0x33}, len(payload)), 0); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := frozen.ReadAt(context.Background(), got, 0); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("mutable RAM leaked into snapshot", err)
	}
	run, err := frozen.RunAt(2<<20, 2<<20)
	if err != nil || run.Kind() != sparse.Hole {
		t.Fatal("hole changed", run, err)
	}
	if _, err := frozen.file.WriteAt([]byte{1}, 0); !errors.Is(err, unix.EPERM) {
		t.Fatal("snapshot not sealed", err)
	}
	if _, err := freezeResumeEarlySourceFD(context.Background(), src, 987654); !errors.Is(err, unix.EBADF) {
		t.Fatal("bad source accepted", err)
	}
}

type resumeEarlyTestPin struct {
	sparse.Source
	closed bool
}

func (p *resumeEarlyTestPin) Close() error { p.closed = true; return nil }

func TestResumeEarlyPinnedDiskViewAndFailureCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "RAM failure"}[fail], func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "ram")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := f.Truncate(4096); err != nil {
				t.Fatal(err)
			}
			source, _ := sparse.NewSource(bytes.NewReader(bytes.Repeat([]byte{0x11}, 4096)), 4096, nil)
			pin := &resumeEarlyTestPin{Source: source}
			created, deleted, opened := "", false, false
			d := DiskDiff{
				TemporaryCheckpoint: func(_ context.Context, id string) error { created = id; return nil },
				OpenCheckpoint: func(_ context.Context, id string) (ExportView, error) {
					if id != created || id == "" {
						t.Fatal(id)
					}
					opened = true
					return pin, nil
				},
				DeleteCheckpoint: func(_ context.Context, id string) error {
					if (opened && !pin.closed) || id != created {
						t.Fatal("deleted before pin closed")
					}
					deleted = true
					return nil
				},
				Export: func(context.Context) (ExportView, error) { t.Fatal("active disk was copied"); return nil, nil },
			}
			s := Sources{MemfdFD: int(f.Fd()), MemfdSize: 4096, Diffs: []DiskDiff{d}}
			if fail {
				s.MemfdFD = 987654
			}
			release, err := freezeResumeEarlyInputs(context.Background(), &s)
			if fail {
				if err == nil || opened || !deleted {
					t.Fatal("failure leaked pin/checkpoint", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opened || pin.closed || deleted {
				t.Fatal("early release")
			}
			view, err := s.Diffs[0].Export(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := view.Close(); err != nil {
				t.Fatal(err)
			}
			if !pin.closed {
				t.Fatal("sink did not release its reader pin")
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			if !pin.closed || !deleted {
				t.Fatal("not released")
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
