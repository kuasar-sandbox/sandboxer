package versioned

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

type payloadRead struct {
	offset int64
	size   int
}

// The observer always reads from the real O_DIRECT file description. It records
// payload syscalls, excluding metadata reads and the higher-level ReadAt calls.
func observePayloadReads(s *Store) *[]payloadRead {
	calls := new([]payloadRead)
	s.mu.Lock()
	s.preadData = func(fd int, b []byte, off int64) (int, error) {
		*calls = append(*calls, payloadRead{off, len(b)})
		return unix.Pread(fd, b, off)
	}
	s.mu.Unlock()
	return calls
}

func denseReadFixture(t *testing.T, pages int, key *[32]byte) (*Store, []byte) {
	t.Helper()
	s, _ := newTestStore(t, pages, key)
	want := make([]byte, pages*PageSize)
	for i := 0; i < pages; i++ {
		copy(want[i*PageSize:], pageFor(uint64(i), byte(i%251+1)))
	}
	if n, err := s.WriteAt(want, 0); err != nil || n != len(want) {
		t.Fatalf("seed: %d %v", n, err)
	}
	flags, err := unix.FcntlInt(s.file.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_DIRECT == 0 {
		t.Fatalf("payload file is not O_DIRECT: %#x %v", flags, err)
	}
	return s, want
}

func TestActiveReadBatchesPhysicalRuns(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			var key *[32]byte
			if encrypted {
				key = &[32]byte{9, 2, 4}
			}
			for _, pages := range []int{64, 600} {
				t.Run(fmt.Sprintf("pages=%d", pages), func(t *testing.T) {
					s, want := denseReadFixture(t, pages, key)
					calls := observePayloadReads(s)
					before := s.Stats()
					got := make([]byte, len(want))
					n, err := s.ReadAt(got, 0)
					if err != nil || n != len(got) || !bytes.Equal(got, want) {
						t.Fatalf("dense read: %d %v equal=%v", n, err, bytes.Equal(got, want))
					}
					if pages == 64 && len(*calls) != 1 {
						t.Fatalf("one contiguous write batch used %d payload reads, want 1", len(*calls))
					}
					if len(*calls) > 20 {
						t.Fatalf("dense read used %d payload syscalls for %d pages", len(*calls), pages)
					}
					// Verify no call crosses a descriptor or includes an index page. A
					// contiguous slot ID alone is insufficient at an allocator group edge.
					dataPages := make(map[uint64]bool, pages)
					s.mu.Lock()
					for block := 0; block < pages; block++ {
						slot, lookupErr := lookup(storeReader{s}, s.ctrl.Root, uint64(block))
						if lookupErr != nil {
							s.mu.Unlock()
							t.Fatal(lookupErr)
						}
						dataPages[slotPage(slot)] = true
					}
					s.mu.Unlock()
					total := 0
					for _, call := range *calls {
						if call.offset%PageSize != 0 || call.size%PageSize != 0 || call.size > 1<<20 {
							t.Fatalf("unaligned or unbounded read: %+v", call)
						}
						total += call.size
						for off := call.offset; off < call.offset+int64(call.size); off += PageSize {
							if !dataPages[uint64(off/PageSize)] {
								t.Fatalf("read included non-data page %d", off/PageSize)
							}
						}
					}
					if total != len(want) || s.Stats().DataRead-before.DataRead != uint64(total) {
						t.Fatalf("completed payload accounting: calls=%d stats=%d want=%d", total, s.Stats().DataRead-before.DataRead, len(want))
					}
				})
			}
		})
	}
}

func TestActiveReadFragmentedHolesAndPartial(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			var key *[32]byte
			if encrypted {
				key = &[32]byte{7, 1, 9}
			}
			s, _ := newTestStore(t, 14, key)
			want := make([]byte, 14*PageSize)
			var writes []Page
			// Physical order deliberately differs from logical order. Block 4 is
			// allocated zero data; it must still be read (and decrypted with XTS).
			for _, block := range []uint64{2, 3, 8, 9, 4, 5, 12, 11, 10} {
				data := pageFor(block, 31)
				if block == 4 {
					clear(data)
				}
				writes = append(writes, Page{block, data})
				copy(want[int(block)*PageSize:], data)
			}
			if err := s.WritePages(ctx, writes); err != nil {
				t.Fatal(err)
			}
			calls := observePayloadReads(s)
			got := bytes.Repeat([]byte{0xee}, len(want))
			if n, err := s.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, want) {
				t.Fatalf("fragmented content: %d %v equal=%v", n, err, bytes.Equal(got, want))
			}
			var sizes []int
			for _, call := range *calls {
				sizes = append(sizes, call.size/PageSize)
			}
			if !reflect.DeepEqual(sizes, []int{2, 2, 2, 1, 1, 1}) {
				t.Fatalf("physical read runs: %v", sizes)
			}
			s.mu.Lock()
			slot, err := lookup(storeReader{s}, s.ctrl.Root, 4)
			s.mu.Unlock()
			if err != nil || slot == 0 {
				t.Fatalf("allocated zero lost presence: %d %v", slot, err)
			}
			for _, tc := range []struct {
				name    string
				off     int64
				size, n int
				err     error
			}{
				{"unaligned", 2*PageSize + 37, 7*PageSize + 43, 7*PageSize + 43, nil},
				{"partial last page", 12*PageSize + 17, 2 * PageSize, 2*PageSize - 17, io.EOF},
				{"hole", 19, PageSize, PageSize, nil},
				{"empty at end", s.Size(), 0, 0, nil},
				{"at end", s.Size(), 1, 0, io.EOF},
				{"past end", s.Size() + 1, 1, 0, io.EOF},
				{"negative", -1, 1, 0, io.EOF},
			} {
				t.Run(tc.name, func(t *testing.T) {
					buf := bytes.Repeat([]byte{0xee}, tc.size)
					n, err := s.ReadAt(buf, tc.off)
					if n != tc.n || !errors.Is(err, tc.err) {
						t.Fatalf("read: %d %v, want %d %v", n, err, tc.n, tc.err)
					}
					if n > 0 && !bytes.Equal(buf[:n], want[int(tc.off):int(tc.off)+n]) {
						t.Fatal("partial content mismatch")
					}
					if !bytes.Equal(buf[n:], bytes.Repeat([]byte{0xee}, len(buf)-n)) {
						t.Fatal("bytes beyond progress changed")
					}
				})
			}
		})
	}
}

func TestActiveReadShortIOKeepsCompletedPrefix(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			var key *[32]byte
			if encrypted {
				key = &[32]byte{3, 7, 2}
			}
			s, want := denseReadFixture(t, 64, key)
			// Warm metadata, then shorten the real file inside the third data page.
			// The O_DIRECT syscall completes two whole pages and a partial third page.
			s.mu.Lock()
			first, err := lookup(storeReader{s}, s.ctrl.Root, 0)
			if err == nil {
				err = s.file.Truncate(int64(slotPage(first)+2)*PageSize + 137)
			}
			s.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			calls := observePayloadReads(s)
			before := s.Stats()
			got := bytes.Repeat([]byte{0xee}, 4*PageSize)
			n, err := s.ReadAt(got, 33)
			if n != 2*PageSize-33 || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("short read: %d %v", n, err)
			}
			if !bytes.Equal(got[:n], want[33:33+n]) {
				t.Fatal("completed prefix content mismatch")
			}
			if !bytes.Equal(got[n:], bytes.Repeat([]byte{0xee}, len(got)-n)) {
				t.Fatal("incomplete page or later bytes overwritten")
			}
			if len(*calls) != 1 || s.Stats().DataRead-before.DataRead != 2*PageSize {
				t.Fatalf("short read accounting: %v %+v", *calls, s.Stats())
			}
			if !errors.Is(s.Err(), io.ErrUnexpectedEOF) {
				t.Fatalf("short read was not sticky: %v", s.Err())
			}
			if n, err = s.ReadAt(got, 0); n != 0 || !errors.Is(err, io.ErrUnexpectedEOF) || len(*calls) != 1 {
				t.Fatalf("read after fatal: %d %v calls=%d", n, err, len(*calls))
			}
		})
	}
}

func TestActiveReadErrorKeepsCompletedPrefix(t *testing.T) {
	s, _ := newTestStore(t, 6, &[32]byte{2, 5, 9})
	putPages(t, s, []uint64{0, 1, 4, 5, 2, 3}, 71)
	calls := 0
	s.mu.Lock()
	s.preadData = func(fd int, b []byte, off int64) (int, error) {
		calls++
		if calls == 2 {
			n, err := unix.Pread(fd, b[:PageSize], off)
			if err != nil {
				return n, err
			}
			return n, unix.EIO
		}
		return unix.Pread(fd, b, off)
	}
	s.mu.Unlock()
	before := s.Stats()
	got := bytes.Repeat([]byte{0xee}, 6*PageSize)
	n, err := s.ReadAt(got, 0)
	if n != 3*PageSize || !errors.Is(err, unix.EIO) || calls != 2 {
		t.Fatalf("read: %d %v calls=%d", n, err, calls)
	}
	for block := 0; block < 3; block++ {
		if !bytes.Equal(got[block*PageSize:(block+1)*PageSize], pageFor(uint64(block), 71)) {
			t.Fatalf("prefix block %d", block)
		}
	}
	if !bytes.Equal(got[n:], bytes.Repeat([]byte{0xee}, len(got)-n)) {
		t.Fatal("bytes after error changed")
	}
	if s.Stats().DataRead-before.DataRead != uint64(n) || !errors.Is(s.Err(), unix.EIO) {
		t.Fatalf("error accounting/fatal: %+v %v", s.Stats(), s.Err())
	}
}

func TestActiveReadLookaheadErrorKeepsCompletedPrefix(t *testing.T) {
	s, want := denseReadFixture(t, 600, nil)
	s.mu.Lock()
	root, err := readNode(storeReader{s}, s.ctrl.Root)
	if err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	if root.leaf || len(root.entries) < 2 {
		s.mu.Unlock()
		t.Fatal("fixture needs at least two leaves")
	}
	boundary := root.entries[0].key + 1
	corruptPage := slotPage(root.entries[1].slot)
	err = directWrite(s.file, make([]byte, PageSize), int64(corruptPage)*PageSize)
	s.cache.remove(corruptPage)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	calls := observePayloadReads(s)
	got := bytes.Repeat([]byte{0xee}, 2*PageSize)
	off := int64(boundary-1) * PageSize
	n, err := s.ReadAt(got, off)
	if n != PageSize || err == nil || !strings.Contains(err.Error(), "B+ page") {
		t.Fatalf("lookahead read: %d %v", n, err)
	}
	if !bytes.Equal(got[:n], want[int(off):int(off)+n]) || !bytes.Equal(got[n:], bytes.Repeat([]byte{0xee}, PageSize)) {
		t.Fatal("lookahead failure lost prefix or changed later bytes")
	}
	if len(*calls) != 1 || s.Err() == nil {
		t.Fatalf("lookahead progress/fatal: calls=%v error=%v", *calls, s.Err())
	}
}

func TestActiveReadProtectsSlotsAcrossMutationAndGC(t *testing.T) {
	s, original := denseReadFixture(t, 128, &[32]byte{7, 3, 1})
	if err := s.Checkpoint(ctx, "retained"); err != nil {
		t.Fatal(err)
	}
	pinned, err := s.Export(ctx, "retained")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	active := make([]byte, len(original))
	for block := 0; block < 128; block++ {
		copy(active[block*PageSize:], pageFor(uint64(block), 201))
	}
	if _, err := s.WriteAt(active, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCheckpoints(ctx, []string{"retained"}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	calls := 0
	s.mu.Lock()
	s.preadData = func(fd int, b []byte, off int64) (int, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return unix.Pread(fd, b, off)
	}
	s.mu.Unlock()
	type result struct {
		n   int
		err error
	}
	got := make([]byte, len(active))
	readDone := make(chan result, 1)
	go func() { n, err := s.ReadAt(got, 0); readDone <- result{n, err} }()
	<-entered
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("active read released slot protection during payload I/O")
	}
	mutations := []func() error{
		func() error { return s.WritePages(ctx, []Page{{127, pageFor(127, 202)}}) },
		func() error { return s.Discard(ctx, 126*PageSize, PageSize) },
		func() error { _, err := s.GC(ctx, 64); return err },
	}
	started := make(chan struct{}, len(mutations))
	done := make(chan error, len(mutations))
	for _, mutate := range mutations {
		go func() { started <- struct{}{}; done <- mutate() }()
	}
	for range mutations {
		<-started
	}
	select {
	case err := <-done:
		t.Fatalf("mutation escaped active read lock: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	r := <-readDone
	if r.n != len(got) || r.err != nil || !bytes.Equal(got, active) {
		t.Fatalf("concurrent read: %d %v equal=%v", r.n, r.err, bytes.Equal(got, active))
	}
	for range mutations {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	copy(active[127*PageSize:], pageFor(127, 202))
	clear(active[126*PageSize : 127*PageSize])
	if n, err := s.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, active) {
		t.Fatalf("mutated active contents: %d %v", n, err)
	}
	if n, err := pinned.ReadAt(ctx, got, 0); n != len(got) || err != nil || !bytes.Equal(got, original) {
		t.Fatalf("deleted pinned version changed: %d %v", n, err)
	}
}

func TestActiveReadRetriesInterruptedSyscall(t *testing.T) {
	s, want := denseReadFixture(t, 2, nil)
	calls := 0
	s.mu.Lock()
	s.preadData = func(fd int, b []byte, off int64) (int, error) {
		calls++
		if calls == 1 {
			return -1, unix.EINTR
		}
		return unix.Pread(fd, b, off)
	}
	s.mu.Unlock()
	got := make([]byte, len(want))
	n, err := s.ReadAt(got, 0)
	if err != nil || n != len(want) || !bytes.Equal(got, want) || calls != 2 {
		t.Fatalf("interrupted read: n=%d err=%v calls=%d equal=%v", n, err, calls, bytes.Equal(got, want))
	}
}
