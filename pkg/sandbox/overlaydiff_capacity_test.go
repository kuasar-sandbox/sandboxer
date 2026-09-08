package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/vhost"
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
			options := func(encrypted bool) []vhost.BlockCOWOption {
				if encrypted {
					return []vhost.BlockCOWOption{vhost.WithDiffEncryption(key, true)}
				}
				return nil
			}
			dir := t.TempDir()
			template, target := filepath.Join(dir, "template.diff"), filepath.Join(dir, "active.diff")
			seed, err := vhost.OpenBlockCOW(template, nil, vhost.DiffInit{CreateSize: size}, options(tc.encryptedTemplate)...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = seed.Close() })
			payload := bytes.Repeat([]byte{0x5a}, 4096)
			if _, err := seed.WriteAt(payload, size-4096); err != nil {
				t.Fatal(err)
			}
			if err := seed.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(template)
			if err != nil {
				t.Fatal(err)
			}

			plan, err := PrepareDiff(target, "file://"+template, size/2)
			if err != nil {
				t.Fatal(err)
			}
			cow, err := vhost.OpenBlockCOW(target, nil, plan, options(tc.encryptedTarget)...)
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
			if tc.encryptedTarget && info.Size() <= size {
				t.Fatal("encrypted physical size did not include its header")
			}

			// Existing data wins even with an unavailable template and a different base size.
			plan, err = PrepareDiff(target, "file:///unused-template", 2*size)
			if err != nil || !plan.Existing {
				t.Fatalf("existing plan=%+v err=%v", plan, err)
			}
			reopened, err := vhost.OpenBlockCOW(target, nil, plan, options(tc.encryptedTarget)...)
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
			plan, err := PrepareDiff(path, "", base.Size())
			if err != nil {
				t.Fatal(err)
			}
			var options []vhost.BlockCOWOption
			if encrypted {
				options = append(options, vhost.WithDiffEncryption([32]byte{1}, true))
			}
			cow, err := vhost.OpenBlockCOW(path, base, plan, options...)
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
