// Package runidentity pins the runtime PID and its process-associated lock.
package runidentity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// Guard owns one runtime's PID file. The runtime must finish its directory
// cleanup before closing the guard. It must not reopen this file itself:
// closing any descriptor for an inode releases this process's POSIX locks.
type Guard struct {
	mu   sync.Mutex
	dir  string
	path string
	file *os.File
	info os.FileInfo
	once sync.Once
	err  error
}

// Kernel record locks do not reject another acquisition by the same process.
// Check before opening: opening and closing a second FD would drop the owner's
// lock even if that second acquisition were rejected afterwards.
var owners = struct {
	sync.Mutex
	byPath map[string]*Guard
}{byPath: make(map[string]*Guard)}

// Acquire creates/locks <runDir>/<sandboxID>.pid without truncating a competing
// owner's file. It also supports the old same-PID exec launcher: F_SETLK keeps
// that process's existing lock while the runtime pins its own descriptor.
func Acquire(runDir, sandboxID string) (*Guard, error) {
	return acquire(runDir, sandboxID, nil)
}

func acquire(runDir, sandboxID string, afterOpen func()) (*Guard, error) {
	if sandboxID == "" || sandboxID == "." || sandboxID == ".." || filepath.Base(sandboxID) != sandboxID {
		return nil, errors.New("runtime identity: invalid sandbox ID")
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	dirInfo, err := os.Lstat(runDir)
	if err != nil {
		return nil, err
	}
	if !dirInfo.IsDir() {
		return nil, errors.New("runtime identity: run directory is not a directory")
	}
	realDir, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return nil, err
	}
	realDir, err = filepath.Abs(realDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(realDir, sandboxID+".pid")
	owners.Lock()
	defer owners.Unlock()
	if owners.byPath[path] != nil {
		return nil, errors.New("runtime identity: already running in this process")
	}
	// Never open a hard link to an inode already owned under another name.
	// Such aliases are not valid runtime identity files.
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("runtime identity: PID file is not regular")
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
			return nil, errors.New("runtime identity: PID file has multiple links")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*Guard, error) { _ = file.Close(); return nil, err }
	if afterOpen != nil {
		afterOpen()
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	if err := syscall.FcntlFlock(file.Fd(), syscall.F_SETLK, &lock); err != nil {
		return fail(fmt.Errorf("runtime identity: PID file locked: %w", err))
	}
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	named, err := os.Lstat(path)
	if err != nil {
		return fail(fmt.Errorf("runtime identity: PID entry changed: %w", err))
	}
	if !info.Mode().IsRegular() || !os.SameFile(info, named) {
		return fail(errors.New("runtime identity: PID entry changed while acquiring ownership"))
	}
	if err := file.Truncate(0); err != nil {
		return fail(err)
	}
	if _, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return fail(err)
	}
	guard := &Guard{dir: realDir, path: path, file: file, info: info}
	owners.byPath[path] = guard
	return guard, nil
}

// RemoveRunDir only removes the directory while its PID entry still denotes
// this guard. A replaced/missing entry cannot authorize deletion of a new run.
func (g *Guard) RemoveRunDir() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	owners.Lock()
	current := owners.byPath[g.path] == g
	owners.Unlock()
	if !current {
		return nil
	}
	named, err := os.Lstat(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(g.info, named) {
		return nil
	}
	return os.RemoveAll(g.dir)
}

func (g *Guard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.once.Do(func() {
		owners.Lock()
		defer owners.Unlock()
		g.err = g.file.Close()
		if owners.byPath[g.path] == g {
			delete(owners.byPath, g.path)
		}
	})
	return g.err
}
