package main

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

// scrubInitialEnvironment overwrites every matching entry in Linux's original
// environment block, so /proc/<pid>/environ no longer exposes it. Call it after
// capturing the key and calling os.Unsetenv: Go's copied environment and any
// previously inherited child environments are separate from this block.
func scrubInitialEnvironment(name string) error {
	if name == "" || bytes.ContainsAny([]byte(name), "=\x00") {
		return fmt.Errorf("invalid environment variable name")
	}
	initial, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		return fmt.Errorf("read initial environment: %w", err)
	}
	defer clear(initial)
	prefix := []byte(name + "=")
	var mem *os.File
	var base int64
	defer func() {
		if mem != nil {
			_ = mem.Close()
		}
	}()
	for offset := 0; offset < len(initial); {
		end := bytes.IndexByte(initial[offset:], 0)
		if end < 0 {
			end = len(initial)
		} else {
			end += offset
		}
		entry := initial[offset:end]
		if bytes.HasPrefix(entry, prefix) {
			if mem == nil {
				stat, err := os.ReadFile("/proc/self/stat")
				if err != nil {
					return fmt.Errorf("read environment address: %w", err)
				}
				base, err = initialEnvironmentAddress(stat, len(initial))
				if err != nil {
					return err
				}
				mem, err = os.OpenFile("/proc/self/mem", os.O_WRONLY, 0)
				if err != nil {
					return fmt.Errorf("open own environment memory: %w", err)
				}
			}
			// Clear the name as well as the value, leaving only NUL bytes.
			clear(entry)
			if _, err := mem.WriteAt(entry, base+int64(offset)); err != nil {
				return fmt.Errorf("overwrite initial environment entry: %w", err)
			}
		}
		offset = end + 1
	}
	return nil
}

func initialEnvironmentAddress(stat []byte, size int) (int64, error) {
	// Field 2 (comm) can contain spaces and parentheses. The remaining fields
	// start at field 3; env_start and env_end are fields 50 and 51.
	endComm := bytes.LastIndexByte(stat, ')')
	if endComm < 0 {
		return 0, fmt.Errorf("invalid process stat: missing comm")
	}
	fields := bytes.Fields(stat[endComm+1:])
	if len(fields) < 49 {
		return 0, fmt.Errorf("invalid process stat: missing environment bounds")
	}
	start, startErr := strconv.ParseInt(string(fields[47]), 10, 64)
	end, endErr := strconv.ParseInt(string(fields[48]), 10, 64)
	if startErr != nil || endErr != nil || start <= 0 || end < start || end-start != int64(size) {
		return 0, fmt.Errorf("invalid process environment bounds")
	}
	return start, nil
}
