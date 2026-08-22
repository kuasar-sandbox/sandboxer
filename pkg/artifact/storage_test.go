package artifact

import (
	"encoding/hex"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestProcessStorageFixesCustomerKey(t *testing.T) {
	first := [32]byte{1, 2, 3}
	second := [32]byte{9, 8, 7}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(first[:]))
	storage, err := NewProcessStorage(&config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalAuto}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	if storage.LocalCodec() == nil || storage.LocalRequired() {
		t.Fatalf("codec=%T required=%v", storage.LocalCodec(), storage.LocalRequired())
	}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(second[:]))
	got, err := storage.CustomerKeyFunc()()
	if err != nil {
		t.Fatal(err)
	}
	if got != first {
		t.Fatal("customer key changed after process initialization")
	}
}

func TestProcessStorageOffDefersAndFixesKeyResolution(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "invalid")
	storage, err := NewProcessStorage(&config.ManifestConfig{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalOff}})
	if err != nil {
		t.Fatal(err)
	}
	if storage.LocalCodec() != nil || storage.LocalRequired() || storage.CustomerKeyFunc() == nil {
		t.Fatalf("keyFn=%v codec=%T required=%v", storage.CustomerKeyFunc() != nil, storage.LocalCodec(), storage.LocalRequired())
	}
	if err := storage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CustomerKeyFunc()(); err == nil {
		t.Fatal("actual manifest key use accepted invalid key")
	}
	valid := [32]byte{1}
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(valid[:]))
	if _, err := storage.CustomerKeyFunc()(); err == nil {
		t.Fatal("customer key error was not fixed after first resolution")
	}
}

func TestProcessStoragePolicyValidation(t *testing.T) {
	for _, policy := range []manifestcrypto.LocalPolicy{manifestcrypto.LocalAuto, manifestcrypto.LocalRequired} {
		t.Run(string(policy), func(t *testing.T) {
			t.Setenv("MANIFEST_KEY", "")
			cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: policy}}
			if _, err := NewProcessStorage(cfg); err == nil {
				t.Fatal("key-requiring policy accepted a missing customer key")
			}
		})
	}
	t.Run("invalid", func(t *testing.T) {
		cfg := &config.ManifestConfig{Crypto: manifestcrypto.Config{Local: "sometimes"}}
		if _, err := NewProcessStorage(cfg); err == nil {
			t.Fatal("invalid crypto.local policy was accepted")
		}
	})
	t.Run("default off", func(t *testing.T) {
		t.Setenv("MANIFEST_KEY", "invalid")
		storage, err := NewProcessStorage(&config.ManifestConfig{})
		if err != nil {
			t.Fatal(err)
		}
		if storage.LocalCodec() != nil || storage.LocalRequired() {
			t.Fatalf("codec=%T required=%v", storage.LocalCodec(), storage.LocalRequired())
		}
		if err := storage.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProcessStorageLocalOnlyCloseDoesNotDialOrResolveKey(t *testing.T) {
	t.Setenv("MANIFEST_KEY", "invalid")
	storage, err := NewProcessStorage(&config.ManifestConfig{
		Store:  manifest.StoreConfig{Endpoint: "not-a-real-endpoint"},
		Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalOff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("unused lazy storage close: %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
