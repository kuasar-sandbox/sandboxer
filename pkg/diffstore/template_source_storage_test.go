package diffstore

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

func TestProvisioningSourceIndependentOfWorkFormat(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, encryptedTarget := range []bool{false, true} {
			t.Run(fmt.Sprintf("checkpoint=%v/encrypted=%v", checkpoint, encryptedTarget), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "template.raw")
				logical := make([]byte, 4*cowBlockSize)
				copy(logical[cowBlockSize:], bytes.Repeat([]byte{0x6d}, cowBlockSize))
				// Allocated zero bytes remain Data; holes inherit the borrowed base.
				writeSparseTemplate(t, path, logical, []sparse.Extent{{Offset: cowBlockSize, Size: 2 * cowBlockSize}})
				var options []DiskOption
				if encryptedTarget {
					options = append(options, WithDiffEncryption(testDiffKey(91), true))
				}
				if checkpoint {
					options = append(options, WithCheckpoint("source-contract", "base"))
				}
				base := bytes.Repeat([]byte{0xa7}, len(logical))
				disk, err := OpenDisk(filepath.Join(dir, "active"), &fakeReader{data: base}, DiffInit{Template: rawTemplateFactory(path)}, options...)
				if err != nil {
					t.Fatal(err)
				}
				defer disk.Close()
				got := make([]byte, len(logical))
				if _, err = disk.ReadAt(got, 0); err != nil {
					t.Fatal(err)
				}
				want := append([]byte(nil), base...)
				copy(want[cowBlockSize:3*cowBlockSize], logical[cowBlockSize:3*cowBlockSize])
				if !bytes.Equal(got, want) {
					t.Fatal("template Hole/Data lower-mask semantics changed")
				}
				if err = disk.WaitMaterialized(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err = disk.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSingleTemplatePresencePublicationIsBatched(t *testing.T) {
	const size = 130 * cowBlockSize
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted=%v", encrypted), func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			input := filepath.Join(dir, "template.ext4")
			want := bytes.Repeat([]byte{0x53}, size)
			writeSparseTemplate(t, input, want, []sparse.Extent{{Offset: 0, Size: size}})
			source, err := openTestRawTemplate(input)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			var options []DiskOption
			if encrypted {
				options = append(options, WithDiffEncryption(testDiffKey(81), true))
			}
			path := filepath.Join(dir, "active")
			disk, err := OpenDisk(path, nil, DiffInit{CreateSize: size}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer disk.Close()
			// Foreground pages split the first template batch into disjoint runs.
			foreground := bytes.Repeat([]byte{0xa1}, cowBlockSize)
			for _, block := range []int{1, 63} {
				if _, err := disk.WriteAt(foreground, int64(block*cowBlockSize)); err != nil {
					t.Fatal(err)
				}
				copy(want[block*cowBlockSize:], foreground)
			}
			if err := disk.Drain(ctx); err != nil {
				t.Fatal(err)
			}
			backend := disk.diff.(*diffFile)
			writes := 0
			backend.beforePresenceWrite = func(int) error { writes++; return nil }
			staging := make([]byte, disk.diff.policy().materialPages*cowBlockSize)
			if err := disk.materializeRange(ctx, source, 0, size, staging); err != nil {
				t.Fatal(err)
			}
			if writes != 3 {
				t.Fatalf("130 pages with foreground gaps published presence %d times, want 3 bounded batches", writes)
			}
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenDisk(path, nil, DiffInit{Existing: true}, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := make([]byte, size)
			if _, err := reopened.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("reopened template/foreground bytes differ: %v", err)
			}
		})
	}
}
