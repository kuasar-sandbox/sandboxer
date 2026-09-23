// Package runidentity pins the runtime PID and its process-associated lock.
package runidentity

import (
	"errors"
	"fmt"
	"io"
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
	mu        sync.Mutex
	dir       string
	path      string
	file      *os.File
	directory *os.File
	info      os.FileInfo
	once      sync.Once
	err       error
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
	if owners.byPath[realDir] != nil {
		return nil, errors.New("runtime identity: already running in this process")
	}
	// Cleanup owns the entire RunDir, not just one logical SandboxID. Lock
	// the existing directory inode so different IDs with the same PathID cannot
	// install competing cleanup owners. This needs no additional lock file.
	dirFD, err := syscall.Open(realDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(dirFD), realDir)
	keepDirectory := false
	defer func() {
		if !keepDirectory {
			_ = directory.Close()
		}
	}()
	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("runtime identity: RunDir is already owned: %w", err)
	}
	openedDir, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	namedDir, err := os.Lstat(realDir)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(dirInfo, openedDir) || !os.SameFile(openedDir, namedDir) {
		return nil, errors.New("runtime identity: RunDir changed while acquiring ownership")
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
	guard := &Guard{dir: realDir, path: path, file: file, directory: directory, info: info}
	owners.byPath[realDir] = guard
	keepDirectory = true
	return guard, nil
}

// RemoveRunDir only removes the directory while its PID entry still denotes
// this guard. A replaced/missing entry cannot authorize deletion of a new run.
func (g *Guard) RemoveRunDir() error {
	return g.removeRunDir(nil)
}

// afterUnlink is a test-only interleaving at the final identity handoff.
func (g *Guard) removeRunDir(afterUnlink func()) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	owners.Lock()
	current := owners.byPath[g.dir] == g
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
	// Keep the identity entry visible and locked until every other entry is
	// gone. RemoveAll(dir) could unlink the PID first, allowing a successor to
	// acquire a new inode while the old recursive deletion is still running.
	directory, err := os.Open(g.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if entry.Name() == filepath.Base(g.path) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(g.dir, entry.Name())); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	named, err = os.Lstat(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(g.info, named) {
		return nil
	}
	if err := os.Remove(g.path); err != nil {
		return err
	}
	if afterUnlink != nil {
		afterUnlink()
	}
	// The PID entry is no longer visible. Never recursively delete from here:
	// a successor may already have created its own PID or other runtime files.
	if err := os.Remove(g.dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
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
		g.err = errors.Join(g.file.Close(), g.directory.Close())
		if owners.byPath[g.dir] == g {
			delete(owners.byPath, g.dir)
		}
	})
	return g.err
}
