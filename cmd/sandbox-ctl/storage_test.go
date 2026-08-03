package main

import (
	"encoding/hex"
	"testing"

	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestStorageOptionsFixesCustomerKey(t *testing.T) {
	first := [32]byte{1, 2, 3}
	second := [32]byte{9, 8, 7}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(first[:]))
	cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalAuto}}
	keyFn, codec, required, err := storageOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if codec == nil || required {
		t.Fatalf("codec=%T required=%v", codec, required)
	}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(second[:]))
	got, err := keyFn()
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatal("customer key changed after process initialization")
	}
}

func TestStorageOptionsOffDoesNotParseKey(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "invalid")
	cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalOff}}
	keyFn, codec, required, err := storageOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if codec != nil || required || keyFn == nil {
		t.Fatalf("keyFn=%v codec=%T required=%v", keyFn != nil, codec, required)
	}
	if _, err := keyFn(); err == nil {
		t.Fatal("actual manifest key use accepted invalid key")
	}
	valid := [32]byte{1}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(valid[:]))
	if _, err := keyFn(); err == nil {
		t.Fatal("customer key error was not fixed after first resolution")
	}
}

func TestStorageOptionsPolicyValidation(t *testing.T) {
	for _, policy := range []manifestcrypto.LocalPolicy{manifestcrypto.LocalAuto, manifestcrypto.LocalRequired} {
		t.Run(string(policy), func(t *testing.T) {
			t.Setenv("MANIFEST_KEY", "")
			cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: policy}}
			if _, _, _, err := storageOptions(cfg); err == nil {
				t.Fatal("key-requiring policy accepted a missing customer key")
			}
		})
	}
	t.Run("invalid", func(t *testing.T) {
		cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: "sometimes"}}
		if _, _, _, err := storageOptions(cfg); err == nil {
			t.Fatal("invalid crypto.local policy was accepted")
		}
	})
	t.Run("default off", func(t *testing.T) {
		t.Setenv("MANIFEST_KEY", "invalid")
		cfg := &config.ManifestConfig{}
		if _, codec, required, err := storageOptions(cfg); err != nil || codec != nil || required {
			t.Fatalf("default policy codec=%T required=%v err=%v", codec, required, err)
		}
	})
}

func TestOnDemandFetcherCloseDoesNotResolveKey(t *testing.T) {
	calls := 0
	f := &onDemandManifestFetcher{
		cfg: &config.ManifestConfig{},
		keyFn: func() ([32]byte, error) {
			calls++
			return [32]byte{}, nil
		},
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("customer key calls=%d, want 0", calls)
	}
}
