package vhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

type inspectingWritebackBody struct {
	diffBodyIO
	inspect func([]byte, int64) error
}

func (b inspectingWritebackBody) WriteAt(p []byte, off int64) (int, error) {
	if err := b.inspect(p, off); err != nil {
		return 0, err
	}
	return b.diffBodyIO.WriteAt(p, off)
}

func TestCOWWritebackOwnedWorkspace(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("encrypted=%t/fail=%t", encrypted, fail), func(t *testing.T) {
				gate := newWorkerGate()
				defer gate.open()
				cache := testCache(t, maxWritebackPages+1, maxWritebackPages+1, cacheHooks{beforeSelect: gate.wait})
				var opts []BlockCOWOption
				if encrypted {
					opts = append(opts, WithDiffEncryption(testDiffKey(41), true))
				}
				cow := cachedCOW(t, cache, maxWritebackPages+1, nil, opts...)
				want := patternedBytes((maxWritebackPages + 1) * cowBlockSize)
				cow.diff.bodyIO = inspectingWritebackBody{cow.diff.bodyIO, func(p []byte, off int64) error {
					if &p[0] != &cow.diff.direct.write.bytes[0] || len(p) > maxDiffScratchSize {
						return fmt.Errorf("body write does not own the bounded aligned workspace")
					}
					start := int(off - cow.diff.bodyOffset)
					if bytes.Equal(p, want[start:start+len(p)]) == encrypted {
						return fmt.Errorf("body encoding mismatch")
					}
					cache.mu.Lock()
					defer cache.mu.Unlock()
					for pos := start; pos < start+len(p); pos += cowBlockSize {
						page := cache.pages[cacheKey{cow, int64(pos / cowBlockSize)}]
						if page == nil || page.state != cacheWriteback || !bytes.Equal(page.data[:], want[pos:pos+cowBlockSize]) {
							return fmt.Errorf("writeback changed or released its frozen plaintext")
						}
					}
					if fail {
						return io.ErrShortWrite
					}
					return nil
				}}
				if _, err := cow.WriteAt(want, 0); err != nil {
					t.Fatal(err)
				}
				gate.open()
				err := cow.Drain(context.Background())
				if fail {
					if !errors.Is(err, io.ErrShortWrite) {
						t.Fatalf("writeback failure = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				cow.diff.direct.writeMu.Lock()
				cleared := allZero(cow.diff.direct.write.bytes)
				cow.diff.direct.writeMu.Unlock()
				if !cleared {
					t.Fatal("writeback retained plaintext/ciphertext in workspace")
				}
				if !fail {
					got := make([]byte, len(want))
					if _, err := cow.diff.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
						t.Fatalf("encoded batch round trip: %v", err)
					}
				}
			})
		}
	}
}
