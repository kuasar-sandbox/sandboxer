package sandbox

import (
	"bytes"
	"context"

	"github.com/kuasar-sandbox/accelerator/pkg/manifest/crypto"
	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
	"github.com/kuasar-sandbox/accelerator/pkg/tarstream"
	"github.com/kuasar-sandbox/sandboxer/pkg/artifact"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore"
)

type capacityBase struct{ *bytes.Reader }

func (capacityBase) Close() error { return nil }

func TestPrepareDiffPreservesTemplateAndExistingLogicalCapacity(t *testing.T) {
	const size int64 = 8 * 4096
	for _, tc := range []struct {
		name                               string
		encryptedTemplate, encryptedTarget bool
	}{
		{"plaintext", false, false},
		{"plaintext-template-encrypted-target", false, true},
		{"encrypted-template-encrypted-target", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := [32]byte{1}
			options := func(encrypted bool) []diffstore.DiskOption {
				if encrypted {
					return []diffstore.DiskOption{diffstore.WithDiffEncryption(key, true)}
				}
				return nil
			}
			dir := t.TempDir()
			template, target := filepath.Join(dir, "template.diff"), filepath.Join(dir, "active.diff")
			payload := bytes.Repeat([]byte{0x5a}, 4096)
			body := make([]byte, size)
			copy(body[1024+0x38:], []byte{0x53, 0xef})
			copy(body[size-4096:], payload)
			templateURI := "file://" + template
			var storage *artifact.ProcessStorage
			if tc.encryptedTemplate {
				codec, err := crypto.NewTarStreamCodec(key)
				if err != nil {
					t.Fatal(err)
				}
				storage, err = artifact.NewProcessStorageWithLocalAccess(nil, func() ([32]byte, error) { return key, nil }, codec, true)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = storage.Close() })
				file, err := os.Create(template)
				if err != nil {
					t.Fatal(err)
				}
				scheme, digest, err := tarstream.WriteTo(context.Background(), file, "image", sparse.Dense(bytes.NewReader(body), uint64(size)), tarstream.WithCodec(codec, true))
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("encode template: %v %v", err, closeErr)
				}
				templateURI += "@" + scheme + ":" + digest
			} else if err := os.WriteFile(template, body, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(template)
			if err != nil {
				t.Fatal(err)
			}

			plan, err := PrepareDiffWithStorage(context.Background(), target, templateURI, nil, storage, nil)
			if err != nil {
				t.Fatal(err)
			}
			cow, err := diffstore.OpenDisk(target, nil, plan, options(tc.encryptedTarget)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cow.Close() })
			if cow.Size() != size {
				t.Fatalf("capacity=%d, want template logical size=%d", cow.Size(), size)
			}
			if _, err := cow.WriteAt([]byte{1}, size); err == nil {
				t.Fatal("out-of-capacity write accepted")
			}
			got := make([]byte, len(payload))
			if _, err := cow.ReadAt(got, size-4096); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("template payload: %v", err)
			}
			if err := cow.Close(); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if logical, err := diffstore.ExistingDiffCapacity(target, options(tc.encryptedTarget)...); err != nil || logical != size {
				t.Fatalf("runtime capacity projection=%d/%v", logical, err)
			}
			if tc.encryptedTarget && info.Size() <= size {
				t.Fatal("encrypted physical size did not include its header")
			}

			// Existing data wins even with an unavailable template and a different base size.
			plan, err = PrepareDiffWithStorage(context.Background(), target, "file:///unused-template", nil, nil, nil)
			if err != nil || !plan.Existing {
				t.Fatalf("existing plan=%+v err=%v", plan, err)
			}
			reopened, err := diffstore.OpenDisk(target, nil, plan, options(tc.encryptedTarget)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if reopened.Size() != size {
				t.Fatalf("existing capacity changed to %d", reopened.Size())
			}
			if _, err := reopened.ReadAt(got, size-4096); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("existing payload: %v", err)
			}
			after, err := os.ReadFile(template)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("template changed: %v", err)
			}
		})
	}
}

func TestPrepareDiffInheritsBaseLogicalCapacity(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x35}, 4*4096)
			base := capacityBase{bytes.NewReader(payload)}
			path := filepath.Join(t.TempDir(), "active.diff")
			plan, err := PrepareDiffWithStorage(context.Background(), path, "", base, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var options []diffstore.DiskOption
			if encrypted {
				options = append(options, diffstore.WithDiffEncryption([32]byte{1}, true))
			}
			cow, err := diffstore.OpenDisk(path, base, plan, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			if cow.Size() != base.Size() || plan.CreateSize != base.Size() {
				t.Fatalf("capacity=%d plan=%+v base=%d", cow.Size(), plan, base.Size())
			}
			got := make([]byte, len(payload))
			if _, err := cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("base payload: %v", err)
			}
		})
	}
}
