package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"golang.org/x/sys/unix"
)

const cliProvenanceConfig = "launch: {systemd: auto, env: {PATH: /image/bin:/usr/bin}, plugin: [{exec: /envd, env: {PATH: /image/bin:/usr/bin}}]}\n"

func cliProvenanceEnvelope(t *testing.T) []byte {
	t.Helper()
	cfg, err := config.LoadConfigBytes([]byte(cliProvenanceConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Launch.SetSystemdPathDefault(true)
	cfg.Launch.Plugin[0].SetSystemdPathDefault(true)
	body, err := config.MarshalConfigProvenance([]byte(cliProvenanceConfig), cfg.Launch)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func inheritProvenanceForTest(t *testing.T, body []byte) int {
	t.Helper()
	fd, err := unix.MemfdCreate("config-provenance-test", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "config-provenance-test")
	defer file.Close()
	if _, err := file.Write(body); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		t.Fatal(err)
	}
	// Leave the shared offset at EOF: the receiving CLI reads the whole sealed
	// file from offset zero without relying on the parent's current position.
	owned, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigProvenanceFDEnv, strconv.Itoa(owned))
	return owned
}

func TestConfigProvenanceFDConsumedClosedAndAppliedBeforeFrom(t *testing.T) {
	envelope := cliProvenanceEnvelope(t)
	fd := inheritProvenanceForTest(t, envelope)
	body, err := consumeConfigProvenance()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(envelope) {
		t.Fatal("FD payload changed")
	}
	if _, exists := os.LookupEnv(config.ConfigProvenanceFDEnv); exists {
		t.Fatal("internal FD env leaked")
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
		t.Fatal("consumed FD remains open")
	}
	path := filepath.Join(t.TempDir(), "sandbox.yaml")
	if err := os.WriteFile(path, []byte(cliProvenanceConfig), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, presence, err := loadRunConfig(path, true, body)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Launch.SystemdPathDefault() || !cfg.Launch.Plugin[0].SystemdPathDefault() || !presence.Has("launch.plugin") {
		t.Fatal("load lost trusted origin or field presence")
	}
	if _, _, err := loadRunConfig(path+":"+path, true, body); err == nil {
		t.Fatal("accepted multiple documents against one digest")
	}
	if err := os.WriteFile(path, []byte(cliProvenanceConfig+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRunConfig(path, true, body); err == nil {
		t.Fatal("accepted stale config bytes")
	}
}

func TestConfigProvenanceFDRejectsInvalidSizeAndStreams(t *testing.T) {
	for _, body := range [][]byte{nil, make([]byte, config.MaxConfigProvenanceBytes+1)} {
		fd := inheritProvenanceForTest(t, body)
		if _, err := consumeConfigProvenance(); err == nil {
			t.Fatal("accepted invalid envelope size")
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil {
			t.Fatal("invalid FD was not closed")
		}
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	fd, err := unix.FcntlInt(read.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.ConfigProvenanceFDEnv, strconv.Itoa(fd))
	if _, err := consumeConfigProvenance(); err == nil {
		t.Fatal("accepted an open stream instead of finite metadata")
	}
	for _, raw := range []string{"", "0", "2", "-1", "4294967296", "n/a"} {
		t.Setenv(config.ConfigProvenanceFDEnv, raw)
		if _, err := consumeConfigProvenance(); err == nil {
			t.Fatal("accepted invalid descriptor", raw)
		}
	}
}

func TestRunRestoreRejectsConfigProvenanceBeforeSourceAccess(t *testing.T) {
	inheritProvenanceForTest(t, cliProvenanceEnvelope(t))
	code, diagnostic := captureStderr(t, func() int {
		return runCmd([]string{"--restore", "unavailable.snapshot", "--config", "unavailable.yaml", "--ch-binary", "unavailable-ch"})
	})
	if code != 2 || !strings.Contains(diagnostic, "provenance is not permitted with --restore") {
		t.Fatalf("restore=%d: %s", code, diagnostic)
	}
}

func TestConfigProvenanceInheritedChild(t *testing.T) {
	if os.Getenv("SANDBOX_TEST_PROVENANCE_CHILD") != "1" {
		return
	}
	body, err := consumeConfigProvenance()
	if err != nil {
		t.Fatal(err)
	}
	cfg, presence, err := loadRunConfig(os.Getenv("SANDBOX_TEST_PROVENANCE_CONFIG"), true, body)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Launch.SystemdPathDefault() || !cfg.Launch.Plugin[0].SystemdPathDefault() || !presence.Has("launch.plugin") {
		t.Fatal("inherited origin did not reach the loaded launch")
	}
	if _, present := os.LookupEnv(config.ConfigProvenanceFDEnv); present {
		t.Fatal("internal descriptor env survived load")
	}
	if _, err := unix.FcntlInt(3, unix.F_GETFD, 0); err == nil {
		t.Fatal("child retained its provenance descriptor")
	}
}

func TestConfigProvenanceSealedMemfdSurvivesExec(t *testing.T) {
	envelope := cliProvenanceEnvelope(t)
	fd, err := unix.MemfdCreate("config-provenance-exec", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "config-provenance-exec")
	defer file.Close()
	if _, err := file.Write(envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK|unix.F_SEAL_SEAL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sandbox.yaml")
	if err := os.WriteFile(path, []byte(cliProvenanceConfig), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConfigProvenanceInheritedChild$")
	child.Env = append(os.Environ(), "SANDBOX_TEST_PROVENANCE_CHILD=1", "SANDBOX_TEST_PROVENANCE_CONFIG="+path, config.ConfigProvenanceFDEnv+"=3")
	child.ExtraFiles = []*os.File{file}
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("inherited provenance: %v\n%s", err, output)
	}
}
