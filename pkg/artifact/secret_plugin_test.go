package artifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/manifest/fetch"
	"github.com/kuasar-sandbox/accelerator/pkg/secret"
	"github.com/kuasar-sandbox/sandboxer/pkg/config"
	"github.com/kuasar-sandbox/sandboxer/pkg/sandboxfile"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshot"
	"github.com/kuasar-sandbox/sandboxer/pkg/snapshotfile"
)

// The default secret client binds process configuration once. A child process
// exercises that production entry point without sharing its configuration with
// other tests or a subsequent -count iteration.
func TestSecretPluginArtifactRoundTrip(t *testing.T) {
	const child = "KUASAR_ARTIFACT_SECRET_TEST"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSecretPluginArtifactRoundTrip$", "-test.count=1")
		cmd.Env = append(os.Environ(), child+"=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("secret plugin artifact subprocess: %v\n%s", err, output)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := [32]byte{0x11, 0x22, 0x33}
	rotated := [32]byte{0x99, 0x88, 0x77}
	const pluginKey = "tenant-a/manifest-v1"
	const reference = "@kms/" + pluginKey
	var mu sync.Mutex
	current := key
	calls := make(map[string]int)
	socket := filepath.Join(t.TempDir(), "secret.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/secrets/resolve" {
			http.Error(w, "invalid operation", http.StatusNotFound)
			return
		}
		var request struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls[request.Key]++
		value := current
		mu.Unlock()
		var raw []byte
		switch request.Key {
		case pluginKey:
			raw = value[:]
		case "short":
			raw = []byte{1, 2, 3}
		default:
			http.Error(w, "secret unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key": request.Key, "value": "base64:" + base64.StdEncoding.EncodeToString(raw),
		})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	cfg, _ := manifestPublisherFixture(t)
	cfg.Crypto.Local = manifestcrypto.LocalRequired
	cfg.Crypto.Ref = manifestcrypto.LocalRequired
	// Keep a valid native fallback in configuration. A failed environment
	// reference must never silently select it.
	cfg.Manifest.Key = hex.EncodeToString(key[:])
	t.Setenv(secret.Env, "kms::"+socket+":cache_ttl=0")
	t.Setenv("MANIFEST_KEY", reference)
	storage, err := NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	assertCalls := func(want int) {
		t.Helper()
		mu.Lock()
		got := calls[pluginKey]
		mu.Unlock()
		if got != want {
			t.Fatalf("plugin key resolved %d times, want %d", got, want)
		}
	}
	assertCalls(1)
	if os.Getenv("MANIFEST_KEY") != reference || cfg.Manifest.Key != hex.EncodeToString(key[:]) {
		t.Fatal("resolved key was written back to environment or configuration")
	}

	// Even with the SDK cache disabled, all consumers of this storage must
	// keep its original key after the broker starts returning another value.
	mu.Lock()
	current = rotated
	mu.Unlock()
	disk := bytes.Repeat([]byte{0x41}, 8192)
	memory := bytes.Repeat([]byte{0x52}, 16384)
	runtime, _ := publishPortable(t, "")
	eSource, err := sandboxfile.BuildSource(publishSource(t, disk), nil, runtime)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewManifestPublisher(storage, cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	e, err := remote.PublishSource(ctx, RoleSandbox, eSource)
	if err != nil {
		t.Fatal(err)
	}
	sConfig, err := snapshot.MarshalConfig(&snapshot.Config{Version: 1, SandboxRef: e.Ref})
	if err != nil {
		t.Fatal(err)
	}
	sSource, err := snapshotfile.BuildSource(publishSource(t, memory), []byte("{}"), []byte("{}"), sConfig)
	if err != nil {
		t.Fatal(err)
	}
	s, err := remote.PublishSource(ctx, RoleSnapshot, sSource, e.Ref)
	if err != nil {
		t.Fatal(err)
	}

	locations := config.RefLocations{"exported": t.TempDir(), "bundle": t.TempDir()}
	local, err := NewLocationPublisher(storage, "exported", locations["exported"], locations, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	localE, err := local.Publish(ctx, e.Ref)
	if err != nil {
		t.Fatal(err)
	}
	localS, err := local.Publish(ctx, s.Ref)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := cfg.WriteAdmission(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewSingleRootBundlePublisher(cfg, storage.CustomerKeyFunc(), admission, "bundle", locations["bundle"], nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	bundledS, err := bundle.PublishSource(ctx, RoleSnapshot, sSource, e.Ref)
	if err != nil {
		t.Fatal(err)
	}

	check := func(reader *ProcessStorage, published PublishResult, want []byte) {
		t.Helper()
		ref, err := manifest.ParseRef(published.Ref)
		if err != nil {
			t.Fatal(err)
		}
		var stream fetch.Stream
		if ref.Scheme == manifest.RefSchemeManifest {
			id, err := manifest.ParseHexKey(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			stream, err = reader.Fetcher().OpenManifest(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			path, err := locations.ResolveFile(ref, "")
			if err != nil {
				t.Fatal(err)
			}
			stream, err = reader.OpenFileWithLocations(ctx, path, ref, locations)
			if err != nil {
				t.Fatal(err)
			}
		}
		defer stream.Close()
		var got []byte
		var readErr error
		if published.Role == RoleSandbox {
			root, err := sandboxfile.Open(ctx, stream)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			got, readErr = readPublishSource(ctx, root.Payload)
		} else {
			root, err := snapshotfile.Open(ctx, stream)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			got, readErr = readPublishSource(ctx, root.Memory)
		}
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("artifact %s payload changed: %v", published.Role, readErr)
		}
	}
	for _, result := range []PublishResult{e, localE} {
		check(storage, result, disk)
	}
	for _, result := range []PublishResult{s, localS, bundledS} {
		check(storage, result, memory)
	}
	assertCalls(1)

	// New storage observes a fresh resolution; the existing one stays fixed.
	fresh, err := NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	got, err := fresh.CustomerKeyFunc()()
	if err != nil || got != rotated {
		t.Fatalf("new storage did not bind its new key: %v", err)
	}
	if _, err := fresh.Inspect(ctx, localS.Ref, locations); err == nil {
		t.Fatal("new storage read old ciphertext using a different resolved key")
	}
	assertCalls(2)

	// The same ciphertext remains readable using the equivalent legacy hex
	// key, proving that references do not change the artifact crypto formats.
	t.Setenv("MANIFEST_KEY", hex.EncodeToString(key[:]))
	legacy, err := NewProcessStorage(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	check(legacy, e, disk)
	for _, result := range []PublishResult{s, localS, bundledS} {
		check(legacy, result, memory)
	}
	assertCalls(2)

	// Config references use the same SDK path when the environment is absent.
	t.Setenv("MANIFEST_KEY", "")
	referenceConfig := *cfg
	referenceConfig.Manifest.Key = reference
	configured, err := NewProcessStorage(&referenceConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer configured.Close()
	got, err = configured.CustomerKeyFunc()()
	if err != nil || got != rotated || referenceConfig.Manifest.Key != reference {
		t.Fatalf("configuration reference was not preserved/resolved: %v", err)
	}
	assertCalls(3)

	for _, invalid := range []string{"@kms/unavailable", "@kms/short", "@disabled/key"} {
		t.Setenv("MANIFEST_KEY", invalid)
		if failed, err := NewProcessStorage(cfg); err == nil {
			_ = failed.Close()
			t.Fatalf("reference %s fell back to the valid native key", invalid)
		} else if strings.Contains(err.Error(), hex.EncodeToString(key[:])) {
			t.Fatal("key material appeared in a resolver error")
		}
		if os.Getenv("MANIFEST_KEY") != invalid {
			t.Fatal("failed resolution rewrote the environment")
		}
	}
}
