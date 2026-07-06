package main

import (
	"os"
	"syscall"
	"time"
)

// runQuiesce executes the v1 pre-snapshot cleanup sequence
// (docs/sandbox-init.md §3.4):
//
//  1. sync(2)                                — flush ext4 upperdir dirty
//  2. echo 3 > /proc/sys/vm/drop_caches      — drop page + dentry/inode cache
//
// Errors are logged and **do not abort** the function; quiesce is a
// best-effort step driving cross-instance dedup quality, not a snapshot
// gate. Any failure shows up as lower dedup ratios in observability,
// not as a halted snapshot.
//
// The /tmp tmpfs reset and the application-level signal hook described
// in docs/sandbox-init.md §3.4 (quiesce extension items) are not implemented here.
func runQuiesce() {
	t0 := time.Now()

	syscall.Sync()
	tSync := time.Since(t0)

	tDrop := time.Duration(0)
	if err := writeFile("/proc/sys/vm/drop_caches", []byte("3\n")); err != nil {
		logf("quiesce: drop_caches write failed: %v (continuing)", err)
	} else {
		tDrop = time.Since(t0) - tSync
	}

	logf("quiesce: sync=%dµs drop_caches=%dµs total=%dµs",
		tSync.Microseconds(), tDrop.Microseconds(), time.Since(t0).Microseconds())
}

func writeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
