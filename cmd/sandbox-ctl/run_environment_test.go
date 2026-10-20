package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestScrubInitialEnvironmentVisibleFromParent(t *testing.T) {
	key := strings.Repeat("ab", 32)
	otherKey := strings.Repeat("cd", 32)
	const helper = "SANDBOX_CTL_ENV_SCRUB_TEST=1"
	const keep = "KEEP_ENV=preserve MANIFEST_KEY=inside another value"
	cases := []struct {
		name string
		env  []string
	}{
		{"first", []string{"MANIFEST_KEY=" + key, helper, keep}},
		{"last", []string{helper, keep, "MANIFEST_KEY=" + key}},
		{"empty-first", []string{"MANIFEST_KEY=", helper, keep}},
		{"duplicates", []string{"MANIFEST_KEY=" + key, "SANDBOX_CTL_ENV_SCRUB_TEST=duplicates", "MANIFEST_KEY=" + otherKey, keep, "MANIFEST_KEY="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestScrubInitialEnvironmentHelper$")
			input, err := json.Marshal(tc.env)
			if err != nil {
				t.Fatal(err)
			}
			// os/exec deduplicates Cmd.Env. Bootstrap through execve to test
			// actual duplicate entries in the kernel's initial environment.
			cmd.Env = []string{"SANDBOX_CTL_ENV_SCRUB_TEST=bootstrap", "SANDBOX_CTL_ENV_SCRUB_INPUT=" + string(input)}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = stdin.Close()
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() || scanner.Text() != "ready" {
				t.Fatalf("scrub helper failed: %s %s", scanner.Text(), stderr.String())
			}
			environ, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/environ")
			if err != nil {
				t.Fatal(err)
			}
			entries := bytes.Split(environ, []byte{0})
			for _, entry := range entries {
				if bytes.HasPrefix(entry, []byte("MANIFEST_KEY=")) {
					t.Fatal("parent can still read the initial manifest key entry")
				}
			}
			for _, original := range tc.env {
				value, matched := strings.CutPrefix(original, "MANIFEST_KEY=")
				if matched && value != "" && bytes.Contains(environ, []byte(value)) {
					t.Fatal("parent can still read an original key value")
				}
			}
			if !bytes.Contains(environ, []byte("KEEP_ENV=preserve MANIFEST_KEY=inside another value\x00")) {
				t.Fatal("scrubbing changed an unrelated environment variable")
			}
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Fatalf("scrub helper: %v %s", err, stderr.String())
			}
		})
	}
}

func TestScrubInitialEnvironmentHelper(t *testing.T) {
	if os.Getenv("SANDBOX_CTL_ENV_SCRUB_TEST") == "bootstrap" {
		var initial []string
		if err := json.Unmarshal([]byte(os.Getenv("SANDBOX_CTL_ENV_SCRUB_INPUT")), &initial); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Exec(os.Args[0], os.Args, initial); err != nil {
			t.Fatal(err)
		}
	}
	mode := os.Getenv("SANDBOX_CTL_ENV_SCRUB_TEST")
	if mode != "1" && mode != "duplicates" {
		return
	}
	cached := os.Getenv("MANIFEST_KEY")
	before, err := os.ReadFile("/proc/self/environ")
	if err != nil {
		t.Fatal(err)
	}
	var expectedCached string
	count := 0
	for _, entry := range bytes.Split(before, []byte{0}) {
		if value, matched := bytes.CutPrefix(entry, []byte("MANIFEST_KEY=")); matched {
			count++
			if count == 1 {
				expectedCached = string(value)
			}
		}
	}
	wantCount := 1
	if mode == "duplicates" {
		wantCount = 3
	}
	if count != wantCount {
		t.Fatalf("initial manifest key entries = %d, want %d", count, wantCount)
	}
	clear(before)
	if err := os.Unsetenv("MANIFEST_KEY"); err != nil {
		t.Fatal(err)
	}
	for range 2 { // Repeated cleanup must be harmless.
		if err := scrubInitialEnvironment("MANIFEST_KEY"); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists := os.LookupEnv("MANIFEST_KEY"); exists {
		t.Fatal("manifest key remains in the Go environment")
	}
	if cached != expectedCached {
		t.Fatal("scrubbing changed the cached Go value")
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestInitialEnvironmentAddress(t *testing.T) {
	fields := make([]string, 49)
	for i := range fields {
		fields[i] = "0"
	}
	fields[47], fields[48] = "4096", "8192"
	stat := []byte("123 (name with ) spaces)) " + strings.Join(fields, " "))
	address, err := initialEnvironmentAddress(stat, 4096)
	if err != nil || address != 4096 {
		t.Fatalf("parse environment bounds: got %d, %v", address, err)
	}
	if _, err := initialEnvironmentAddress(stat, 4095); err == nil {
		t.Fatal("accepted an environment size that does not match the kernel bounds")
	}
	if _, err := initialEnvironmentAddress([]byte("invalid"), 1); err == nil {
		t.Fatal("accepted malformed process stat")
	}
}
