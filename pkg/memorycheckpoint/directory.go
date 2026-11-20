package memorycheckpoint

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const holderSocketName = "m.sock"
const holderMarkerName = "identity.json"

type holderMarker struct {
	Version         int            `json:"version"`
	Producer        RunIdentity    `json:"producer"`
	CheckpointID    string         `json:"checkpoint_id"`
	HostOperationID string         `json:"host_operation_id,omitempty"`
	MemoryID        string         `json:"memory_id"`
	Epoch           uint64         `json:"epoch"`
	Holder          HolderIdentity `json:"holder"`
}

func markerFor(r CaptureRecord) holderMarker {
	return holderMarker{r.Version, r.Producer, r.CheckpointID, r.HostOperationID, r.MemoryID, r.Epoch, r.Holder}
}

type holderDirectory struct {
	file    *os.File
	id      HolderDirectoryIdentity
	marker  *holderMarker
	emptyOK bool
}

func fileIdentity(f *os.File) (FileIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return FileIdentity{}, err
	}
	return FileIdentity{Device: uint64(st.Dev), Inode: st.Ino}, nil
}

func openDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func verifyDirectory(f *os.File, id HolderDirectoryIdentity) error {
	got, err := fileIdentity(f)
	if err != nil {
		return err
	}
	var named unix.Stat_t
	if err := unix.Lstat(id.Path, &named); err != nil {
		return err
	}
	if got.Device != id.Device || got.Inode != id.Inode || uint64(named.Dev) != id.Device || named.Ino != id.Inode || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("memory checkpoint: holder directory identity changed")
	}
	return nil
}

func createHolderDirectory(runDir string, r CaptureRecord) (*holderDirectory, error) {
	canonical, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return nil, err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	identity, err := json.Marshal(markerFor(r))
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(identity)
	path, err := os.MkdirTemp(filepath.Dir(canonical), fmt.Sprintf(".km-%x-", hash[:6]))
	if err != nil {
		return nil, err
	}
	f, err := openDirectory(path)
	if err != nil {
		return nil, err // Never remove a directory without owning its guard.
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err // A Runtime may already own the newly created name.
	}
	fi, err := fileIdentity(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	d := &holderDirectory{file: f, id: HolderDirectoryIdentity{Path: path, Device: fi.Device, Inode: fi.Inode}}
	if err = verifyDirectory(f, d.id); err != nil {
		_ = f.Close()
		return nil, err
	}
	return d, nil
}

func (d *holderDirectory) writeMarker(r CaptureRecord) error {
	m := markerFor(r)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	fd, err := unix.Openat(int(d.file.Fd()), holderMarkerName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), holderMarkerName)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		// The file was exclusively created by this guarded preparation.
		_ = unix.Unlinkat(int(d.file.Fd()), holderMarkerName, 0)
		return err
	}
	d.marker = &m
	return d.file.Sync()
}

func readHolderMarker(f *os.File) (*holderMarker, error) {
	fd, err := unix.Openat(int(f.Fd()), holderMarkerName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	mf := os.NewFile(uintptr(fd), holderMarkerName)
	b, err := io.ReadAll(io.LimitReader(mf, 64*1024+1))
	err = errors.Join(err, mf.Close())
	if err != nil {
		return nil, err
	}
	if len(b) > 64*1024 {
		return nil, errors.New("memory checkpoint: oversized holder marker")
	}
	var m holderMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// remove owns the directory flock already. It never traverses RunDir or an
// ancestor, and refuses both changed identity and unrelated directory contents.
func (d *holderDirectory) remove() error {
	if d == nil || d.file == nil {
		return nil
	}
	if err := verifyDirectory(d.file, d.id); err != nil {
		return err
	}
	markerMissing := false
	if d.marker != nil {
		m, err := readHolderMarker(d.file)
		if errors.Is(err, os.ErrNotExist) && d.emptyOK {
			markerMissing = true
		} else if err != nil || *m != *d.marker {
			return errors.Join(errors.New("memory checkpoint: holder marker mismatch"), err)
		}
	}
	entries, err := os.ReadDir(fmt.Sprintf("/proc/self/fd/%d", d.file.Fd()))
	if err != nil {
		return err
	}
	if markerMissing && len(entries) != 0 {
		return errors.New("memory checkpoint: missing holder marker in nonempty directory")
	}
	for _, entry := range entries {
		if entry.Name() == holderSocketName && entry.Type()&os.ModeSocket != 0 {
			continue
		}
		if entry.Name() == holderMarkerName && d.marker != nil && entry.Type().IsRegular() {
			continue
		}
		return fmt.Errorf("memory checkpoint: unexpected holder directory entry %q", entry.Name())
	}
	// Keep the marker until the last unlink. A crash can then leave either an
	// identifiable directory or the exact empty terminal inode, never an
	// unmarked directory that still contains a socket or unrelated contents.
	for _, name := range []string{holderSocketName, holderMarkerName} {
		for _, entry := range entries {
			if entry.Name() != name {
				continue
			}
			if err := unix.Unlinkat(int(d.file.Fd()), name, 0); err != nil {
				return err
			}
			if name == holderMarkerName {
				d.emptyOK = true
			}
		}
	}
	if err := verifyDirectory(d.file, d.id); err != nil {
		return err
	}
	return os.Remove(d.id.Path)
}

func (d *holderDirectory) close() error {
	if d == nil || d.file == nil {
		return nil
	}
	err := d.remove()
	err = errors.Join(err, d.file.Close())
	d.file = nil
	return err
}

// RemoveHolderDirectory cleans only the exact generation in the durable record.
// An absent directory is idempotent; replacement, contention and extra contents
// are errors. No runtime directory or ancestor is ever inspected or removed.
func RemoveHolderDirectory(r CaptureRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	f, err := openDirectory(r.Holder.Directory.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := verifyDirectory(f, r.Holder.Directory); err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("memory checkpoint: holder directory remains owned: %w", err)
	}
	m := markerFor(r)
	d := &holderDirectory{file: f, id: r.Holder.Directory, marker: &m, emptyOK: r.RAMReleased || r.Aborted}
	return d.remove()
}
