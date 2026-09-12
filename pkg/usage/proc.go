package usage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type procStat struct {
	PID                        int
	Comm                       string
	Start, User, System, Guest uint64
	Threads                    int
}

func parseProcStat(raw []byte) (procStat, error) {
	s := string(raw)
	left, right := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if left < 2 || right <= left || right+2 >= len(s) || s[right+1] != ' ' {
		return procStat{}, errors.New("usage: malformed proc stat comm")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:left]))
	if err != nil || pid <= 0 {
		return procStat{}, errors.New("usage: invalid proc PID")
	}
	fields := strings.Fields(s[right+2:]) // field 3 (state) starts after the final ')'.
	if len(fields) < 41 || len(fields[0]) != 1 {
		return procStat{}, errors.New("usage: incomplete proc stat")
	}
	out := procStat{PID: pid, Comm: s[left+1 : right]}
	for _, v := range []struct {
		field int
		dst   *uint64
	}{{14, &out.User}, {15, &out.System}, {22, &out.Start}, {43, &out.Guest}} {
		*v.dst, err = strconv.ParseUint(fields[v.field-3], 10, 64)
		if err != nil {
			return procStat{}, fmt.Errorf("usage proc stat field %d: %w", v.field, err)
		}
	}
	out.Threads, err = strconv.Atoi(fields[20-3])
	if err != nil || out.Threads < 1 {
		return procStat{}, errors.New("usage: invalid proc thread count")
	}
	return out, nil
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("usage: oversized proc input")
	}
	return b, nil
}

// clockTicks uses the kernel-provided ELF auxiliary vector, not CONFIG_HZ,
// libc, cgo or a subprocess. Both supported Linux architectures use ELF64 LE.
func clockTicks() (uint64, error) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return 0, errors.New("usage: unsupported auxv architecture")
	}
	b, err := readBounded("/proc/self/auxv", 32*1024)
	if err != nil {
		return 0, err
	}
	return parseAuxv(b, 8, binary.LittleEndian)
}

func parseAuxv(b []byte, word int, order binary.ByteOrder) (uint64, error) {
	if (word != 4 && word != 8) || len(b)%(2*word) != 0 {
		return 0, errors.New("usage: malformed auxv")
	}
	get := func(b []byte) uint64 {
		if word == 4 {
			return uint64(order.Uint32(b))
		}
		return order.Uint64(b)
	}
	var hertz uint64
	terminated := false
	for off := 0; off < len(b); off += 2 * word {
		key, value := get(b[off:]), get(b[off+word:])
		if terminated {
			if key != 0 || value != 0 {
				return 0, errors.New("usage: data after AT_NULL")
			}
			continue
		}
		if key == 0 {
			if value != 0 {
				return 0, errors.New("usage: invalid AT_NULL")
			}
			terminated = true
			continue
		}
		if key == 17 { // AT_CLKTCK, include/uapi/linux/auxvec.h
			if hertz != 0 || value == 0 || value > 1e9 {
				return 0, errors.New("usage: invalid/repeated AT_CLKTCK")
			}
			hertz = value
		}
	}
	if !terminated || hertz == 0 {
		return 0, errors.New("usage: AT_CLKTCK or AT_NULL missing")
	}
	return hertz, nil
}

func parseRSS(raw []byte) (anon, file uint64, err error) {
	seenAnon, seenFile := false, false
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || (f[0] != "RssAnon:" && f[0] != "RssFile:") {
			continue
		}
		if len(f) != 3 || f[2] != "kB" {
			return 0, 0, errors.New("usage: invalid RSS unit")
		}
		n, e := strconv.ParseUint(f[1], 10, 64)
		if e != nil || n > ^uint64(0)/1024 {
			return 0, 0, errors.New("usage: invalid RSS value")
		}
		if f[0] == "RssAnon:" {
			if seenAnon {
				return 0, 0, errors.New("usage: duplicate RssAnon")
			}
			anon, seenAnon = n*1024, true
		} else {
			if seenFile {
				return 0, 0, errors.New("usage: duplicate RssFile")
			}
			file, seenFile = n*1024, true
		}
	}
	if !seenAnon || !seenFile {
		return 0, 0, errors.New("usage: RSS fields missing")
	}
	return anon, file, nil
}

type vcpuThread struct {
	tid   int
	start uint64
}

var errAmbiguousVCPU = errors.New("usage: ambiguous vCPU mapping")

type procReader struct {
	root, boot  string
	hertz       uint64
	read        func(string, int64) ([]byte, error)
	readDir     func(string) ([]os.DirEntry, error)
	threads     map[int]vcpuThread
	threadCount int
	vcpuCount   int
	// Only the CH proc execution slot accesses this bounded validity state.
	// A discarded result cannot make a confirmed source loss complete again.
	incomplete []bool
}

func newProcReader(vcpus int) (*procReader, error) {
	hertz, err := clockTicks()
	if err != nil {
		return nil, err
	}
	boot, err := readBounded("/proc/sys/kernel/random/boot_id", 128)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(boot))
	if len(id) != 36 {
		return nil, errors.New("usage: invalid host boot identity")
	}
	return &procReader{root: "/proc", boot: id, hertz: hertz, read: readBounded, readDir: os.ReadDir,
		threads: make(map[int]vcpuThread), vcpuCount: vcpus}, nil
}

func (p *procReader) stat(path string) (procStat, error) {
	b, err := p.read(path, 16*1024)
	if err != nil {
		return procStat{}, err
	}
	return parseProcStat(b)
}
func (p *procReader) identity(s procStat) string {
	return fmt.Sprintf("%s/%d/%d", p.boot, s.PID, s.Start)
}

func (p *procReader) discover(pid, count int) error {
	path := filepath.Join(p.root, strconv.Itoa(pid), "task")
	entries, err := p.readDir(path)
	if err != nil {
		return err
	}
	found := make(map[int]vcpuThread, p.vcpuCount)
	for _, entry := range entries {
		tid, e := strconv.Atoi(entry.Name())
		if e != nil || tid <= 0 {
			continue
		}
		s, e := p.stat(filepath.Join(path, entry.Name(), "stat"))
		if e != nil {
			// An unrelated topology change can force discovery while a known
			// thread's stat temporarily fails. Keep only its previously checked
			// identity; readProcess revalidates it before accepting any ticks.
			if !errors.Is(e, os.ErrNotExist) {
				for cpu, cached := range p.threads {
					if cached.tid == tid {
						if _, exists := found[cpu]; exists {
							return errAmbiguousVCPU
						}
						found[cpu] = cached
					}
				}
			}
			continue
		}
		if !strings.HasPrefix(s.Comm, "vcpu") {
			continue
		}
		cpu, e := strconv.Atoi(strings.TrimPrefix(s.Comm, "vcpu"))
		if e != nil || cpu < 0 || cpu >= p.vcpuCount || s.Comm != "vcpu"+strconv.Itoa(cpu) {
			continue
		}
		if _, exists := found[cpu]; exists {
			return errAmbiguousVCPU
		}
		found[cpu] = vcpuThread{tid, s.Start}
	}
	p.threads, p.threadCount = found, count
	return nil
}
