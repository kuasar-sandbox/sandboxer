package memorycheckpoint

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// PrepareHolder consumes memory on every return. A successful Holder owns all
// descriptors independently of Runtime/Storage. Failed publication retains only
// its record writer and GC pin until the precommit abort is durable.
func PrepareHolder(directory, runDir string, initial CaptureRecord, memory *os.File, size uint64, logf func(string, ...any)) (_ *Holder, retErr error) {
	return prepareHolder(directory, runDir, initial, memory, size, logf, nil)
}

// beforePublish is a per-preparation test barrier for real publication failures;
// it never alters a process-global writer or another capture's ownership.
func prepareHolder(directory, runDir string, initial CaptureRecord, memory *os.File, size uint64, logf func(string, ...any), beforePublish func()) (_ *Holder, retErr error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	h := &Holder{file: memory, size: size, readers: make(map[string]*readerLease), ready: make(chan struct{}), done: make(chan struct{}), changed: make(chan struct{}, 1), logf: logf}
	defer func() {
		if retErr == nil {
			return
		}
		if h.record != nil && h.record.published {
			retErr = errors.Join(retErr, h.Abort())
		} else {
			h.finish(nil)
			retErr = errors.Join(retErr, h.err)
		}
	}()
	if memory == nil || size == 0 || initial.Version != Version || !validID(initial.MemoryID) || !validCheckpointID(initial.CheckpointID) || initial.Epoch == 0 || initial.Producer.SandboxID == "" || initial.Committed || initial.Aborted || initial.Discarded || initial.RuntimeRetired || initial.FileReady || initial.RAMReleased || initial.SnapshotRef != "" || initial.SandboxRef != "" || initial.PayloadRef != "" {
		return nil, errors.New("memory checkpoint: invalid holder preparation")
	}
	flags, err := unix.FcntlInt(memory.Fd(), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_RDONLY {
		return nil, errors.New("memory checkpoint: holder requires read-only memory fd")
	}
	st, err := memory.Stat()
	if err != nil {
		return nil, err
	}
	if uint64(st.Size()) != size {
		return nil, errors.New("memory checkpoint: memfd size mismatch")
	}
	h.gcPin, err = PinDirectory(directory)
	if err != nil {
		return nil, err
	}
	start, err := ProcessStartTime(os.Getpid())
	if err != nil {
		return nil, err
	}
	initial.Holder = HolderIdentity{PID: os.Getpid(), StartTime: start}
	h.directory, err = createHolderDirectory(runDir, initial)
	if err != nil {
		return nil, err
	}
	initial.Holder.Directory = h.directory.id
	initial.Holder.SocketPath = filepath.Join(h.directory.id.Path, holderSocketName)
	h.record, err = reserveRecord(directory, initial)
	if err != nil {
		return nil, err
	}
	initial = h.record.Read()
	h.ramPin, initial.Holder.RAMLock, err = createRAMPin(h.record.Path())
	if err != nil {
		return nil, err
	}
	h.record.record = initial
	if err := h.directory.writeMarker(initial); err != nil {
		return nil, err
	}
	bindPath := fmt.Sprintf("/proc/self/fd/%d/%s", h.directory.file.Fd(), holderSocketName)
	h.listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: bindPath, Net: "unix"})
	if err != nil {
		return nil, err
	}
	h.listener.SetUnlinkOnClose(false)
	if err := os.Chmod(initial.Holder.SocketPath, 0600); err != nil {
		return nil, err
	}
	if beforePublish != nil {
		beforePublish()
	}
	if err := publishRecord(directory, h.record); err != nil {
		return nil, err
	}
	go h.serve()
	return h, nil
}

// The stable pin is never truncated or replaced. A previous failed preparation
// can leave an unreferenced inode; the writer lock plus EX proves it can be reused.
func createRAMPin(recordPath string) (*os.File, FileIdentity, error) {
	f, err := os.OpenFile(recordPath+".ram.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, FileIdentity{}, err
	}
	failed := func(err error) (*os.File, FileIdentity, error) {
		return nil, FileIdentity{}, errors.Join(err, f.Close())
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return failed(errors.Join(errors.New("memory checkpoint: invalid RAM pin"), err))
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return failed(err)
	}
	id, err := fileIdentity(f)
	if err != nil {
		return failed(err)
	}
	st, err := f.Stat()
	if err != nil {
		return failed(err)
	}
	nonce := make([]byte, 32)
	if st.Size() == 0 {
		if _, err := rand.Read(nonce); err != nil {
			return failed(err)
		}
		if _, err := f.WriteAt(nonce, 0); err != nil {
			return failed(err)
		}
	} else if st.Size() != int64(len(nonce)) {
		return failed(errors.New("memory checkpoint: invalid RAM pin nonce"))
	} else if _, err := f.ReadAt(nonce, 0); err != nil {
		return failed(err)
	}
	id.Nonce = hex.EncodeToString(nonce)
	if err := f.Sync(); err != nil {
		return failed(err)
	}
	d, err := openDirectory(filepath.Dir(recordPath))
	if err != nil {
		return failed(err)
	}
	err = errors.Join(d.Sync(), d.Close())
	if err != nil {
		return failed(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return failed(err)
	}
	return f, id, nil
}

func openRAMPin(recordPath string, expected FileIdentity, mode int) (*os.File, error) {
	if expected.Inode == 0 {
		return nil, errors.New("memory checkpoint: missing RAM proof identity")
	}
	f, err := os.OpenFile(recordPath+".ram.lock", os.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err = validateRAMPin(f, expected); err == nil {
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func validateRAMPin(f *os.File, expected FileIdentity) error {
	if expected.Nonce == "" {
		return errors.New("memory checkpoint: missing RAM proof nonce")
	}
	id, err := fileIdentity(f)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if id.Device != expected.Device || id.Inode != expected.Inode || !st.Mode().IsRegular() {
		return errors.New("memory checkpoint: RAM proof inode mismatch")
	}
	if expected.Nonce != "" {
		want, err := hex.DecodeString(expected.Nonce)
		if err != nil || len(want) != 32 || st.Size() != 32 {
			return errors.New("memory checkpoint: RAM proof nonce mismatch")
		}
		got := make([]byte, 32)
		if _, err := io.ReadFull(io.NewSectionReader(f, 0, 32), got); err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return errors.New("memory checkpoint: RAM proof nonce mismatch")
		}
	}
	return nil
}
