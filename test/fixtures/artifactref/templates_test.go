package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest"
	manifestcrypto "github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
	"github.com/kuasar-sandbox/sandboxer/pkg/emptyext4"
)

func TestPreparedTemplateCarriers(t *testing.T) {
	ctx := context.Background()
	source, err := emptyext4.New(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, source.Size())
	if _, err := source.ReadAt(ctx, body, 0); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw := filepath.Join(dir, "raw.ext4")
	if err := os.WriteFile(raw, body, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"template-carriers", raw, dir}, &output); err != nil {
		t.Fatal(err)
	}
	var refs map[string]string
	if err := json.Unmarshal(output.Bytes(), &refs); err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(refs["customer_key"])
	if err != nil || len(decoded) != 32 {
		t.Fatal(err)
	}
	var key [32]byte
	copy(key[:], decoded)
	storage, err := artifact.NewProcessStorageWithCustomerKey(&manifest.Config{Crypto: manifestcrypto.Config{Local: manifestcrypto.LocalAuto, Chunk: "aes", Manifest: "aes"}}, func() ([32]byte, error) { return key, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	for _, name := range []string{"raw", "plain", "encrypted", "bundle"} {
		t.Run(name, func(t *testing.T) {
			opened, err := storage.OpenDiffTemplate(ctx, refs[name], "", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			got := make([]byte, len(body))
			if _, err := opened.ReadAt(ctx, got, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, body) {
				t.Fatal("carrier changed the logical disk")
			}
			if verifier, ok := opened.(diffstore.TemplateVerifier); ok {
				if err := verifier.Verify(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
