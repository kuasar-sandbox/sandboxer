package main

import (
	"os"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

// runQuiesce executes the v1 pre-snapshot cleanup sequence
// (docs/sandbox-init.md §3.4):
//
//  1. sync(2)                           — flush ext4 upperdir dirty
//  2. optional drop_caches write        — drop page + dentry/inode cache
//
// Sync always runs. The drop is skipped only when the host explicitly asks;
// otherwise errors are logged and reported but **do not abort** the function.
// Cache cleanup is a best-effort dedup aid, not a snapshot gate.
//
// The /tmp tmpfs reset and the application-level signal hook described
// in docs/sandbox-init.md §3.4 (quiesce extension items) are not implemented here.
func runQuiesce(skipDropCaches bool) proto.DropCachesResult {
	return runQuiesceWith(skipDropCaches, syscall.Sync, writeFile)
}

func runQuiesceWith(skipDropCaches bool, syncFn func(), writeFn func(string, []byte) error) proto.DropCachesResult {
	t0 := time.Now()

	syncFn()
	tSync := time.Since(t0)

	tDrop := time.Duration(0)
	result := proto.DropCachesSkipped
	if skipDropCaches {
		logf("quiesce: drop_caches skipped by snapshot request")
	} else if err := writeFn("/proc/sys/vm/drop_caches", []byte("3\n")); err != nil {
		result = proto.DropCachesFailed
		logf("quiesce: drop_caches write failed: %v (continuing)", err)
	} else {
		result = proto.DropCachesSucceeded
		tDrop = time.Since(t0) - tSync
	}

	logf("quiesce: sync=%dµs drop_caches=%dµs result=%s total=%dµs",
		tSync.Microseconds(), tDrop.Microseconds(), result, time.Since(t0).Microseconds())
	return result
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
