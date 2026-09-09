// usageprobe is a static guest workload for e2e_usage.sh. It is not shipped.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func fail(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func number(s string) int {
	n, err := strconv.Atoi(s)
	fail(err)
	if n < 0 {
		panic("negative amount")
	}
	return n
}
func main() {
	if len(os.Args) < 2 {
		panic("usageprobe MODE [ARGS]")
	}
	switch os.Args[1] {
	case "wait":
		fmt.Println("USAGE-PROBE-READY")
		for {
			if _, err := os.Stat("/tmp/usage-exit"); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	case "exit":
		fail(os.WriteFile("/tmp/usage-exit", nil, 0600))
	case "true":
	case "cpu":
		end := time.Now().Add(time.Duration(number(os.Args[2])) * time.Second)
		var n uint64
		for time.Now().Before(end) {
			for i := 0; i < 100000; i++ {
				n = n*1664525 + 1013904223
			}
		}
		fmt.Println(n)
	case "memory":
		b := make([]byte, number(os.Args[2])*1024*1024)
		for i := range b {
			b[i] = byte(i*17 + 1)
		}
		fmt.Printf("MEMORY-READY %d\n", len(b))
		time.Sleep(time.Duration(number(os.Args[3])) * time.Second)
		runtime.KeepAlive(b)
	case "write":
		f, err := os.Create(os.Args[2])
		fail(err)
		b := make([]byte, 1024*1024)
		for i := range b {
			b[i] = byte(i*17 + 1)
		}
		for i := 0; i < number(os.Args[3]); i++ {
			_, err = f.Write(b)
			fail(err)
		}
		fail(f.Sync())
		fail(f.Close())
	case "statfs":
		var s syscall.Statfs_t
		fail(syscall.Statfs(os.Args[2], &s))
		fail(json.NewEncoder(os.Stdout).Encode(s))
	case "inspect":
		out := map[string]string{}
		init, err := os.ReadFile("/proc/1/exe")
		fail(err)
		out["sandbox_init_sha256"] = fmt.Sprintf("%x", sha256.Sum256(init))
		for _, name := range []string{"version", "zoneinfo", "buddyinfo", "meminfo", "self/status", "self/stat"} {
			b, err := os.ReadFile("/proc/" + name)
			fail(err)
			out[name] = string(b)
		}
		out["balloon_proc_field"] = strconv.FormatBool(strings.Contains(out["meminfo"], "Balloon"))
		out["page_size"] = strconv.Itoa(os.Getpagesize())
		fail(json.NewEncoder(os.Stdout).Encode(out))
	default:
		panic("unknown workload")
	}
}
