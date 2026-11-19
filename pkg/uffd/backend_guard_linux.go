package uffd

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const backendGuardEnv = "SANDBOX_UFFD_GUARD_BACKEND_PAGE"
const backendGuardPrefix = "uffd-backend-guard "

// UFFD_USER_MODE_ONLY permits an unprivileged missing-user-fault diagnostic.
// Kernel uaccess into a missing guarded page can fail with EFAULT instead of
// delivering SIGBUS to Go. No kernel-fault handling is enabled by this guard.
const backendGuardUserModeOnly = 1

type backendGuardRecord struct {
	Version      int    `json:"v"`
	Sequence     int    `json:"seq"`
	Event        string `json:"event"`
	Reason       string `json:"reason"`
	PID          int    `json:"pid"`
	Page         uint64 `json:"page"`
	TargetValid  bool   `json:"target_valid"`
	BackendVA    uint64 `json:"backend_va"`
	TargetVA     uint64 `json:"target_va"`
	Length       uint64 `json:"length"`
	HostPageSize int    `json:"host_page_size"`
	MemfdFD      int    `json:"memfd_fd"`
	MemfdDev     uint64 `json:"memfd_dev"`
	MemfdIno     uint64 `json:"memfd_ino"`
	MemfdSize    int64  `json:"memfd_size"`
	GuardFD      int    `json:"guard_fd"`
	Errno        int    `json:"errno"`
	Elapsed      uint64 `json:"elapsed_ns"`
}

// BackendPageGuard is an opt-in diagnostic of one missing 4 KiB backend page.
// Its independent UFFD delivers SIGBUS on a user access to the still-absent
// backing page; it does not read memory, drain faults, or service CH's UFFD.
// Another alias or fd can populate the shared page first, so a silent guard
// does not establish that every backend access passed through EnsureLoaded.
type BackendPageGuard struct {
	fd       int
	record   backendGuardRecord
	started  time.Time
	logf     func(string, ...any)
	close    sync.Once
	closeErr error
}

// ArmBackendPageGuardFromEnv registers the backend alias only when
// SANDBOX_UFFD_GUARD_BACKEND_PAGE names a complete, aligned 4 KiB memfd page.
// The setting is a decimal byte offset or an explicit 0x hexadecimal offset.
// Empty means off, with no syscall or log. Invalid or unavailable diagnostics
// produce bounded numeric records and return nil, without changing startup's
// result. The caller must arm before exposing memory to any backend consumer,
// and Close only after all such consumers have joined, before memfd unmap.
func ArmBackendPageGuardFromEnv(memfdFD int, backendVA uintptr, size int, logf func(string, ...any)) *BackendPageGuard {
	value := os.Getenv(backendGuardEnv)
	if value == "" {
		return nil
	}
	if logf == nil {
		logf = log.Printf
	}
	g := &BackendPageGuard{
		fd: -1, started: time.Now(), logf: logf,
		record: backendGuardRecord{
			Version: 1, PID: os.Getpid(), BackendVA: uint64(backendVA),
			Length: PageSize, HostPageSize: os.Getpagesize(),
			MemfdFD: memfdFD, MemfdSize: int64(size), GuardFD: -1,
		},
	}
	page, valid := backendGuardPage(value, size)
	if valid && backendVA != 0 && backendVA%PageSize == 0 &&
		uint64(backendVA) <= uint64(^uintptr(0))-page-PageSize {
		g.record.Page, g.record.TargetVA, g.record.TargetValid = page, uint64(backendVA)+page, true
	}
	g.emit("attempt", "success", nil)
	fail := func(reason string, err error) *BackendPageGuard {
		if g.fd >= 0 {
			// Linux releases the descriptor even if close reports an error.
			// Retrying could close a descriptor reused by another goroutine.
			_ = unix.Close(g.fd)
		}
		g.emit("failed", reason, err)
		return nil
	}
	if !valid {
		return fail("target", unix.EINVAL)
	}
	if g.record.HostPageSize != PageSize {
		return fail("page_size", unix.EOPNOTSUPP)
	}
	if !g.record.TargetValid {
		return fail("address", unix.EINVAL)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(memfdFD, &stat); err != nil {
		return fail("stat", err)
	}
	g.record.MemfdDev, g.record.MemfdIno = uint64(stat.Dev), stat.Ino
	if stat.Size != int64(size) {
		return fail("stat", unix.EINVAL)
	}
	fd, err := createUffd(unix.O_CLOEXEC | backendGuardUserModeOnly)
	if err != nil {
		return fail("create", err)
	}
	g.fd, g.record.GuardFD = fd, fd
	// SIGBUS handles missing faults without a reader. In particular, do not
	// request REMOVE/UNMAP/FORK events, whose acknowledgement could block a
	// producer when no reader exists. CH keeps its separate normal UFFD.
	const requested = uffdFeatureMissingShmem | uffdFeatureSigbus
	features, err := ioctlUffdAPI(fd, requested)
	if err != nil {
		return fail("api", err)
	}
	if features&requested != requested {
		return fail("features", unix.EOPNOTSUPP)
	}
	if err := ioctlUffdRegister(fd, g.record.TargetVA, PageSize); err != nil {
		return fail("register", err)
	}
	g.emit("armed", "success", nil)
	return g
}

func backendGuardPage(value string, size int) (uint64, bool) {
	if value == "" || len(value) > 20 || size < PageSize {
		return 0, false
	}
	base, digits := 10, value
	if len(value) >= 2 && value[0] == '0' && (value[1] == 'x' || value[1] == 'X') {
		base, digits = 16, value[2:]
	}
	if digits == "" {
		return 0, false
	}
	for _, c := range digits {
		if c >= '0' && c <= '9' || base == 16 && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			continue
		}
		return 0, false
	}
	page, err := strconv.ParseUint(digits, base, 64)
	return page, err == nil && page%PageSize == 0 && page <= uint64(size-PageSize)
}

// Close unregisters the guard by closing its dedicated fd. It is nil-safe and
// idempotent. A fatal SIGBUS does not run defers, so attempt/armed are emitted
// immediately; no close record is expected on that path.
func (g *BackendPageGuard) Close() error {
	if g == nil {
		return nil
	}
	g.close.Do(func() {
		g.closeErr = unix.Close(g.fd)
		g.emit("closed", "close", g.closeErr)
	})
	return g.closeErr
}

func (g *BackendPageGuard) emit(event, reason string, err error) {
	g.record.Sequence++
	g.record.Event, g.record.Reason, g.record.Errno = event, reason, 0
	g.record.Elapsed = uint64(time.Since(g.started))
	if err != nil {
		var errno unix.Errno
		g.record.Errno = -1
		if errors.As(err, &errno) {
			g.record.Errno = int(errno)
		}
	}
	// Only fixed strings and numbers are serialized. The env value, RAM,
	// paths, and error text never enter these at-most-three immediate lines.
	body, err := json.Marshal(g.record)
	if err == nil && len(backendGuardPrefix)+len(body) < 2048 {
		g.logf("%s%s", backendGuardPrefix, body)
	}
}
