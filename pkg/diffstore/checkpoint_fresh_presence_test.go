package diffstore

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func TestCheckpointFreshPresenceAndReopen(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, test := range []struct {
			name     string
			size     int64
			template bool
		}{
			{"empty", 520 * cowBlockSize, false},
			{"maximum", 32 << 30, false},
			{"template-capacity", 520 * cowBlockSize, true},
		} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", test.name, encrypted), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "active.diff")
				base := &fakeReader{data: bytes.Repeat([]byte{0x61}, 4*cowBlockSize)}
				options := []DiskOption{WithCheckpoint("fresh", "base")}
				if encrypted {
					options = append(options, WithDiffEncryption([32]byte{37}, true))
				}
				init := DiffInit{CreateSize: test.size}
				if test.template {
					init.TemplatePath = filepath.Join(dir, "template.raw")
					// The template, rather than CreateSize, determines capacity.
					init.CreateSize = cowBlockSize
					writeSparseTemplate(t, init.TemplatePath, bytes.Repeat([]byte{0x72}, int(test.size)),
						[]sparse.Extent{{Size: 3 * cowBlockSize}})
				}
				cow, err := OpenDisk(path, base, init, options...)
				if err != nil {
					t.Fatal(err)
				}
				defer cow.Close()
				if cow.Size() != test.size || len(cow.bitmap) != int((test.size/cowBlockSize+63)/64) {
					t.Fatalf("wrong capacity/bitmap size: %d/%d", cow.Size(), len(cow.bitmap))
				}
				if !test.template && cow.DirtyCount() != 0 {
					t.Fatal("new disk has upper presence")
				}
				got := make([]byte, cowBlockSize)
				initial := byte(0x61)
				if test.template {
					initial = 0x72
				}
				if _, err := cow.ReadAt(got, 0); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{initial}, cowBlockSize)) {
					t.Fatal("fresh disk lost lower/template content", err)
				}
				data := bytes.Repeat([]byte{0x39}, cowBlockSize)
				last := test.size - cowBlockSize
				for _, off := range []int64{0, last} {
					if _, err := cow.WriteAt(data, off); err != nil {
						t.Fatal(err)
					}
				}
				if err := cow.Discard(cowBlockSize, cowBlockSize); err != nil {
					t.Fatal(err)
				}
				if err := cow.Checkpoint(context.Background(), "saved"); err != nil {
					t.Fatal(err)
				}
				if err := cow.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := OpenDisk(path, base, DiffInit{Existing: true}, options...)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				for _, off := range []int64{0, last} {
					if _, err := reopened.ReadAt(got, off); err != nil || !bytes.Equal(got, data) {
						t.Fatalf("reopen lost Data presence at %d: %v", off, err)
					}
				}
				if _, err := reopened.ReadAt(got, cowBlockSize); err != nil || !bytes.Equal(got, make([]byte, cowBlockSize)) {
					t.Fatal("reopen lost Zero presence", err)
				}
				if test.template {
					if _, err := reopened.ReadAt(got, 2*cowBlockSize); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{0x72}, cowBlockSize)) {
						t.Fatal("reopen lost materialized template content", err)
					}
				}
			})
		}
	}
}

func TestCheckpointFreshFrontendPresenceIsIndependent(t *testing.T) {
	gate := make(chan struct{})
	var release sync.Once
	cache, err := newCache(8*cowBlockSize, 4*cowBlockSize, cacheHooks{
		beforeWrite: func([]*cachePage) error { <-gate; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	cow := checkpointCOW(t, filepath.Join(t.TempDir(), "active.diff"), nil, 64*cowBlockSize, true, WithCache(cache))
	defer cow.Close()
	defer release.Do(func() { close(gate) })
	if _, err := cow.WriteAt(bytes.Repeat([]byte{0x39}, cowBlockSize), 0); err != nil {
		t.Fatal(err)
	}
	persisted, err := cow.versions.store.PresenceBitmap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !cow.blockDirty(0) || persisted[0] != 0 {
		t.Fatal("accepted frontend write changed committed backend presence")
	}
	release.Do(func() { close(gate) })
	if err := cow.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	persisted, err = cow.versions.store.PresenceBitmap(context.Background())
	if err != nil || persisted[0]&1 == 0 {
		t.Fatal("drained write did not reach backend presence", err)
	}
}
