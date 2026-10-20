package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestRunStorageRetainsEnvironmentKey(t *testing.T) {
	for _, policy := range []string{"off", "auto", "required"} {
		t.Run(policy, func(t *testing.T) {
			t.Setenv(manifest.CustomerKeyEnv, strings.Repeat("ab", 32))
			cfg := &config.ManifestConfig{}
			cfg.Manifest.Key = strings.Repeat("cd", 32)
			cfg.Crypto.Local = manifestcrypto.LocalPolicy(policy)
			storage, err := newRunProcessStorage(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			if _, exists := os.LookupEnv(manifest.CustomerKeyEnv); exists {
				t.Fatal("manifest key remains in the process environment")
			}
			if err := exec.Command("/bin/sh", "-c", `test "${MANIFEST_KEY+x}" != x`).Run(); err != nil {
				t.Fatalf("child inherited the manifest key: %v", err)
			}
			want := [32]byte{}
			for i := range want {
				want[i] = 0xab
			}
			for range 2 {
				got, err := storage.CustomerKeyFunc()()
				if err != nil || got != want {
					t.Fatal("process storage did not retain the environment override")
				}
			}
			if cfg.Manifest.Key != strings.Repeat("cd", 32) {
				t.Fatal("environment key was written back to the manifest config")
			}
		})
	}
}

func TestRunStorageWithoutManifestConfigUnsetsEnvironmentKey(t *testing.T) {
	t.Setenv(manifest.CustomerKeyEnv, strings.Repeat("ab", 32))
	storage, err := newRunProcessStorage(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	if _, exists := os.LookupEnv(manifest.CustomerKeyEnv); exists {
		t.Fatal("unused manifest key remains in the process environment")
	}
}

func TestRunStorageLocalOffWithoutKey(t *testing.T) {
	t.Setenv(manifest.CustomerKeyEnv, "")
	cfg := &config.ManifestConfig{}
	cfg.Crypto.Local = manifestcrypto.LocalOff
	storage, err := newRunProcessStorage(cfg)
	if err != nil {
		t.Fatalf("local-only operation unexpectedly requires a key: %v", err)
	}
	defer storage.Close()
}

func TestRunStorageLocalOffRejectsInvalidEnvironmentKey(t *testing.T) {
	t.Setenv(manifest.CustomerKeyEnv, "invalid")
	cfg := &config.ManifestConfig{}
	cfg.Manifest.Key = strings.Repeat("cd", 32)
	cfg.Crypto.Local = manifestcrypto.LocalOff
	if storage, err := newRunProcessStorage(cfg); err == nil {
		_ = storage.Close()
		t.Fatal("invalid environment override was silently replaced with the config key")
	}
}

func TestRunStorageWithoutEnvironmentKeyDoesNotRequireProc(t *testing.T) {
	t.Setenv(manifest.CustomerKeyEnv, "")
	if err := os.Unsetenv(manifest.CustomerKeyEnv); err != nil {
		t.Fatal(err)
	}
	for _, policy := range []manifestcrypto.LocalPolicy{manifestcrypto.LocalOff, manifestcrypto.LocalAuto, manifestcrypto.LocalRequired} {
		t.Run(string(policy), func(t *testing.T) {
			cfg := &config.ManifestConfig{}
			cfg.Manifest.Key = strings.Repeat("cd", 32)
			cfg.Crypto.Local = policy
			storage, err := newRunProcessStorageWithScrub(cfg, func(string) error {
				t.Fatal("YAML-only key unexpectedly requires /proc access")
				return os.ErrPermission
			})
			if err != nil {
				t.Fatal(err)
			}
			defer storage.Close()
			key, err := storage.CustomerKeyFunc()()
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range key {
				if b != 0xcd {
					t.Fatal("YAML key was not preserved")
				}
			}
		})
	}
	storage, err := newRunProcessStorageWithScrub(nil, func(string) error {
		t.Fatal("local-only invocation unexpectedly requires /proc access")
		return os.ErrPermission
	})
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
}

func TestRunStorageEnvironmentKeyScrubFailureIsFatal(t *testing.T) {
	for _, tc := range []struct{ name, value string }{{"set", strings.Repeat("ab", 32)}, {"empty", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(manifest.CustomerKeyEnv, tc.value)
			cfg := &config.ManifestConfig{}
			cfg.Manifest.Key = strings.Repeat("cd", 32)
			cfg.Crypto.Local = manifestcrypto.LocalOff
			called := false
			storage, err := newRunProcessStorageWithScrub(cfg, func(name string) error {
				called = true
				if name != manifest.CustomerKeyEnv {
					t.Fatal("unexpected environment variable selected for scrubbing")
				}
				if _, exists := os.LookupEnv(name); exists {
					t.Fatal("Go environment was not cleared before scrubbing")
				}
				return os.ErrPermission
			})
			if !called || !errors.Is(err, os.ErrPermission) || storage != nil {
				t.Fatalf("scrub failure must prevent startup: called=%v storage=%v err=%v", called, storage != nil, err)
			}
		})
	}
}
