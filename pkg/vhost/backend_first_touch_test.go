package vhost

// Real kernel regression: two MAP_SHARED aliases with only the CH alias
// MISSING_SHMEM registered, exactly as in ServeAndWait. Both access orders
// must preserve the snapshot and advance the saved used index from 7 to 8.
import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/sandboxer/pkg/uffd"
	"golang.org/x/sys/unix"
)

type firstTouchSource struct {
	data  []byte
	reads atomic.Int32
}
type firstTouchRun struct{ source *firstTouchSource }

func (s *firstTouchSource) RunAt(off, limit uint64) (sparse.Run, error) {
	if off >= uint64(len(s.data)) || limit == 0 {
		return nil, fmt.Errorf("invalid first-touch range")
	}
	return firstTouchRun{s}, nil
}
func (r firstTouchRun) Offset() uint64       { return 0 }
func (r firstTouchRun) End() uint64          { return uint64(len(r.source.data)) }
func (r firstTouchRun) Kind() sparse.RunKind { return sparse.Data }
func (r firstTouchRun) ReadAt(ctx context.Context, p []byte, off uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.source.reads.Add(1)
	if off >= uint64(len(r.source.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.source.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func firstTouchIOCTL(fd int, request uintptr, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), request, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}
func TestRealMissingShmemRestoredUsedRingPreservesUntouchedBytes(t *testing.T) {
	for _, cpuFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("cpu_first_%t", cpuFirst), func(t *testing.T) {
			const size = 4096
			memfd, err := unix.MemfdCreate("issue304-first-touch", unix.MFD_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(memfd)
			if err = unix.Ftruncate(memfd, size); err != nil {
				t.Fatal(err)
			}
			backend, err := unix.Mmap(memfd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Munmap(backend)
			cpu, err := unix.Mmap(memfd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Munmap(cpu)
			raw, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK|1), 0, 0)
			// USER_MODE_ONLY was added after the supported 5.10 floor. Fall back
			// only for an unsupported flag; permission/capability failures remain fatal.
			if errno == unix.EINVAL {
				raw, _, errno = unix.Syscall(unix.SYS_USERFAULTFD, uintptr(unix.O_CLOEXEC|unix.O_NONBLOCK), 0, 0)
			}
			if errno != 0 {
				t.Fatalf("real MISSING_SHMEM UFFD required: %v", errno)
			}
			fd := int(raw)
			owned := false
			defer func() {
				if !owned {
					unix.Close(fd)
				}
			}()
			api := struct{ API, Features, IOCTLs uint64 }{API: 0xaa, Features: 1 << 5}
			if err = firstTouchIOCTL(fd, 0xc018aa3f, unsafe.Pointer(&api)); err != nil {
				t.Fatal(err)
			}
			cva := uint64(uintptr(unsafe.Pointer(&cpu[0])))
			bva := uint64(uintptr(unsafe.Pointer(&backend[0])))
			reg := struct{ Start, Length, Mode, IOCTLs uint64 }{Start: cva, Length: size, Mode: 1}
			if err = firstTouchIOCTL(fd, 0xc020aa00, unsafe.Pointer(&reg)); err != nil {
				t.Fatal(err)
			}
			source := &firstTouchSource{data: bytes.Repeat([]byte{0xa5}, size)}
			binary.LittleEndian.PutUint16(source.data[128:130], 0)
			binary.LittleEndian.PutUint16(source.data[130:132], 7)
			amap := uffd.NewAddressMap(size)
			if err = amap.RegisterVMA(uffd.ProcessCH, cva, size, 0); err != nil {
				t.Fatal(err)
			}
			if err = amap.RegisterVMA(uffd.ProcessBackend, bva, size, 0); err != nil {
				t.Fatal(err)
			}
			h, err := uffd.NewWithBackendUffd(fd, amap, uffd.Config{MemfdFD: memfd, BackendVA: uintptr(bva), Size: size, Source: source, NumWorkers: 2, Logf: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			owned = true
			h.Start()
			defer h.Close()
			if cpuFirst {
				if cpu[size-1] != 0xa5 {
					t.Fatal("positive control did not restore source")
				}
				cpu[3000] = 0x3b // Live CPU writes must survive the Loaded fast path.
			}
			var st unix.Stat_t
			if err = unix.Fstat(memfd, &st); err != nil {
				t.Fatal(err)
			}
			regs := []MemRegion{{GuestPhysAddr: 0, MemorySize: size, UserspaceAddr: cva, MmapOffset: 0}}
			if err = BindRegions(regs, []int{memfd}, st.Ino, backend); err != nil {
				t.Fatal(err)
			}
			server := &Server{}
			server.SetMemoryLoader(h.EnsureLoaded)
			server.memTable.SetRegions(regs)
			q := &virtq{num: 8, usedAddr: cva + 128}
			if err = server.publishUsed(q, 3, 17); err != nil {
				t.Fatal(err)
			}
			if source.reads.Load() != 1 {
				t.Fatalf("barrier snapshot reads = %d, want 1 before CPU verification", source.reads.Load())
			}
			want := append([]byte(nil), source.data...)
			if cpuFirst {
				want[3000] = 0x3b
			}
			binary.LittleEndian.PutUint16(want[130:132], 8)
			binary.LittleEndian.PutUint32(want[188:192], 3)
			binary.LittleEndian.PutUint32(want[192:196], 17)
			first := -1
			changed := 0
			for i, b := range cpu {
				if b != want[i] {
					changed++
					if first < 0 {
						first = i
					}
				}
			}
			t.Logf("cpu_first=%t snapshot_reads=%d used_idx=%d differing_bytes=%d first=%d", cpuFirst, source.reads.Load(), binary.LittleEndian.Uint16(cpu[130:132]), changed, first)
			if changed != 0 {
				t.Errorf("restored guest data lost: first=%d actual=%02x expected=%02x; used index=%d expected=8", first, cpu[first], want[first], binary.LittleEndian.Uint16(cpu[130:132]))
			}
		})
	}
}
