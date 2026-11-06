package diffstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/versioned"
)

type presenceCountedBase struct {
	fakeReader
	reads atomic.Int64
}

func (r *presenceCountedBase) ReadAt(p []byte, off int64) (int, error) {
	r.reads.Add(1)
	return r.fakeReader.ReadAt(p, off)
}

// This corruption fixture deliberately knows the on-disk metadata layout. It
// changes logical tree bounds while preserving the B+ page checksum, so the
// regression exercises range validation rather than a checksum rejection.
func corruptCheckpointPresenceBoundary(t *testing.T, path, mode string, encrypted bool) uint64 {
	t.Helper()
	const blocks = 1024
	opts := versioned.Options{DiffID: "disk-under-test", BaseID: "immutable-base", LogicalSize: 2 * blocks * cowBlockSize}
	if encrypted {
		opts.Key = &[32]byte{1, 2, 3}
	}
	store, err := versioned.Create(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	pages := make([]versioned.Page, blocks)
	for i := range pages {
		pages[i] = versioned.Page{Block: uint64(2 * i), Data: bytes.Repeat([]byte{0xc3}, cowBlockSize)}
	}
	if err := store.WritePages(context.Background(), pages); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	readPage := func(offset int64) []byte {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		page := make([]byte, cowBlockSize)
		if _, err := f.ReadAt(page, offset); err != nil {
			t.Fatal(err)
		}
		return page
	}
	slotOffset := func(slot uint64) int64 {
		n := slot - 1
		return int64(2048+(n/128)*129+1+n%128) * cowBlockSize
	}
	rootPage := func() (int64, []byte) {
		control := readPage(cowBlockSize)
		if string(control[:7]) != "KDCONT4" {
			t.Fatal("unexpected control page")
		}
		var fields struct{ Root uint64 }
		n := binary.LittleEndian.Uint32(control[8:])
		if n > cowBlockSize-20 {
			t.Fatal("invalid control JSON length")
		}
		if err := json.Unmarshal(control[12:12+n], &fields); err != nil || fields.Root == 0 {
			t.Fatal("invalid fixture root", err)
		}
		offset := slotOffset(fields.Root)
		page := readPage(offset)
		if string(page[:8]) != "KDBTREE4" || page[10] != 0 || binary.LittleEndian.Uint16(page[8:]) < 2 {
			t.Fatal("fixture must have an internal root with neighboring leaves")
		}
		return offset, page
	}
	offset, page := rootPage()
	count := int(binary.LittleEndian.Uint16(page[8:]))
	var target uint64
	switch mode {
	case "skipped-left-tail":
		target = binary.LittleEndian.Uint64(page[16:])
		binary.LittleEndian.PutUint64(page[16:], target-1)
	case "skipped-final-tail":
		entry := 16 + (count-1)*16
		target = binary.LittleEndian.Uint64(page[entry:])
		binary.LittleEndian.PutUint64(page[entry:], target-1)
	case "skipped-right-low":
		// Choose a real hole in the selected left leaf, then move the next
		// leaf's first mapping across its declared lower boundary into it.
		target = binary.LittleEndian.Uint64(page[16:]) - 1
		if target%2 != 1 {
			t.Fatal("fixture target is not a hole")
		}
		offset = slotOffset(binary.LittleEndian.Uint64(page[40:]))
		page = readPage(offset)
		if string(page[:8]) != "KDBTREE4" || page[10] != 1 || binary.LittleEndian.Uint64(page[16:]) <= target {
			t.Fatal("fixture must have a right leaf above the selected hole")
		}
		binary.LittleEndian.PutUint64(page[16:], target)
	case "leaf-capacity", "leaf-allocation":
		// The requested first entry is intact. Damage a different entry in
		// that leaf, which must be validated even for a complete overwrite.
		offset = slotOffset(binary.LittleEndian.Uint64(page[24:]))
		page = readPage(offset)
		count := int(binary.LittleEndian.Uint16(page[8:]))
		if string(page[:8]) != "KDBTREE4" || page[10] != 1 || count < 2 {
			t.Fatal("fixture must have a populated first leaf")
		}
		target = binary.LittleEndian.Uint64(page[16:])
		other := 16 + (count-1)*16
		if mode == "leaf-capacity" {
			binary.LittleEndian.PutUint64(page[other:], 2*blocks)
		} else {
			binary.LittleEndian.PutUint64(page[other+8:], ^uint64(0))
		}
	default:
		t.Fatal("unknown corruption", mode)
	}
	hash := crc32.NewIEEE()
	hash.Write(page[:12])
	hash.Write(page[16:])
	binary.LittleEndian.PutUint32(page[12:], hash.Sum32())
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(page, offset); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestCheckpointPayloadCorruptionNeverMasqueradesAsZero(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, mode := range []string{"skipped-left-tail", "skipped-final-tail", "skipped-right-low", "leaf-capacity", "leaf-allocation"} {
			for _, operation := range []string{"read", "partial-write", "full-write", "discard"} {
				t.Run(fmt.Sprintf("encrypted=%t/%s/%s", encrypted, mode, operation), func(t *testing.T) {
					const size = 2048 * cowBlockSize
					path := filepath.Join(t.TempDir(), "active.diff")
					target := corruptCheckpointPresenceBoundary(t, path, mode, encrypted)
					base := &presenceCountedBase{fakeReader: fakeReader{data: bytes.Repeat([]byte{0x61}, size)}}
					opts := []DiskOption{WithCheckpoint("disk-under-test", "immutable-base")}
					if encrypted {
						opts = append(opts, WithDiffEncryption([32]byte{1, 2, 3}, true))
					}
					cow, err := OpenDisk(path, base, DiffInit{Existing: true, CreateSize: size}, opts...)
					if err != nil {
						t.Fatal("open should leave descendant validation to payload access", err)
					}
					defer cow.Close()
					backend, err := cow.checkpointBackend()
					if err != nil {
						t.Fatal(err)
					}
					before := backend.store.Stats()
					var reports atomic.Int64
					reported := make(chan struct{}, 1)
					cow.SetFatalHandler(func(error) {
						reports.Add(1)
						select {
						case reported <- struct{}{}:
						default:
						}
					})
					var n int
					switch operation {
					case "read":
						n, err = cow.ReadAt(make([]byte, cowBlockSize), int64(target)*cowBlockSize)
					case "partial-write":
						n, err = cow.WriteAt([]byte{0x99}, int64(target)*cowBlockSize+37)
					case "full-write":
						n, err = cow.WriteAt(bytes.Repeat([]byte{0x99}, cowBlockSize), int64(target)*cowBlockSize)
					case "discard":
						err = cow.Discard(int64(target)*cowBlockSize, cowBlockSize)
					}
					// P=0 is authoritative even if unrelated payload metadata is
					// corrupt. It need not inspect the index to fall through.
					hole := target%2 == 1
					if operation == "read" && hole {
						if err != nil || n != cowBlockSize || base.reads.Load() != 1 || cow.Err() != nil {
							t.Fatal("explicit Hole inspected payload index", n, err, base.reads.Load())
						}
						return
					}
					// Complete writes/discards and partial writes into Hole may
					// be accepted in the cache. Completion must validate before
					// writing payload or replacing/deleting mappings and bitmap.
					accepted := operation == "discard" || operation == "full-write" || operation == "partial-write" && hole
					if accepted {
						if err != nil || operation != "discard" && n == 0 {
							t.Fatal("cache acceptance failed", n, err)
						}
						err = cow.Drain(context.Background())
					} else if n != 0 {
						t.Fatal("failed operation returned bytes", n, err)
					}
					if err == nil {
						t.Fatalf("corrupt payload index became successful %s", operation)
					}
					wantBase := int64(0)
					if operation == "partial-write" && hole {
						wantBase = 1
					}
					if base.reads.Load() != wantBase || cow.BackendStats()["diff_presence_complete"] != true {
						t.Fatal("incorrect base fallback", base.reads.Load(), cow.BackendStats())
					}

					// This fixture has no Current checkpoint to invalidate. A rejected
					// operation must not commit a replacement/deletion of its mappings.
					after := backend.store.Stats()
					if after.DataWritten != before.DataWritten || after.IndexWritten != before.IndexWritten || after.RedoWritten != before.RedoWritten {
						t.Fatalf("rejected %s changed persistent data/index: before=%+v after=%+v", operation, before, after)
					}
					fatal := cow.Err()
					// Failure becomes visible before the worker invokes the handler
					// outside the cache lock. Drain may observe that sticky failure
					// first; wait for the callback before asserting its exact count.
					awaitSignal(t, reported)
					if fatal == nil || !errors.Is(err, fatal) && !errors.Is(fatal, err) || !errors.Is(cow.Flush(), fatal) || reports.Load() != 1 {
						t.Fatal("logical corruption was not sticky", err, cow.Err(), reports.Load())
					}
					if _, next := cow.ReadAt(make([]byte, cowBlockSize), 0); !errors.Is(next, fatal) {
						t.Fatal("read continued after fatal presence", next)
					}
					if _, next := cow.WriteAt(make([]byte, cowBlockSize), 0); !errors.Is(next, fatal) {
						t.Fatal("write continued after fatal presence", next)
					}
				})
			}
		}
	}
}
