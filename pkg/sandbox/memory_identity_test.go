package sandbox

import (
	"encoding/hex"
	"github.com/kuasar-sandbox/sandboxer/pkg/memorycheckpoint"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func fixtureMemoryHolder(t *testing.T, directory, checkpointID, memoryID string, start uint64) memorycheckpoint.HolderIdentity {
	t.Helper()
	path, err := memorycheckpoint.RecordPath(directory, checkpointID, memoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path+".ram.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	if _, err := f.WriteAt(nonce, 0); err != nil {
		t.Fatal(err)
	}
	var ram unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &ram); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	return memorycheckpoint.HolderIdentity{PID: os.Getpid(), StartTime: start, SocketPath: filepath.Join(dir, "m.sock"), Directory: memorycheckpoint.HolderDirectoryIdentity{Path: dir, Device: uint64(st.Dev), Inode: st.Ino}, RAMLock: memorycheckpoint.FileIdentity{Device: uint64(ram.Dev), Inode: ram.Ino, Nonce: hex.EncodeToString(nonce)}}
}
