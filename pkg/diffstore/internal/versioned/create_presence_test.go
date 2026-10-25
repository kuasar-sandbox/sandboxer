package versioned

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/diffstore/internal/presence"
)

func TestCreatePresenceEncodingAndReopen(t *testing.T) {
	for _, pages := range []int{1, 255, 256, 257, presence.Pages(presence.MaxSize)} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("pages=%d/encrypted=%t", pages, encrypted), func(t *testing.T) {
				// Exercise partially used last pages and the maximum capacity.
				size := int64((pages-1)*presence.PayloadSize*8+1) * PageSize
				if pages == presence.Pages(presence.MaxSize) {
					size = presence.MaxSize
				}
				var key *[32]byte
				if encrypted {
					key = &[32]byte{37}
				}
				path := filepath.Join(t.TempDir(), "created.diff")
				s, err := Create(path, Options{DiffID: "created", LogicalSize: size, Key: key})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				empty := make([]byte, presence.Bytes(size))
				for i := 0; i < pages; i++ {
					page := make([]byte, PageSize)
					if _, err := f.ReadAt(page, int64(presenceAreaPage+uint64(i))*PageSize); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(page, presence.Encode(empty, size, i)) {
						t.Fatalf("bitmap page %d changed its encoding or offset", i)
					}
				}
				s, err = Open(path, Options{Key: key})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				bitmap, err := s.PresenceBitmap(ctx)
				if err != nil || !bytes.Equal(bitmap, empty) {
					t.Fatalf("reopened presence: length=%d error=%v", len(bitmap), err)
				}
			})
		}
	}
}

func TestInitialPresenceActualShortWrite(t *testing.T) {
	for _, mode := range []string{"short-initial-presence", "short-initial-presence-tail"} {
		t.Run(mode, func(t *testing.T) {
			cmd := helperCommand(t, mode, filepath.Join(t.TempDir(), "short.diff"))
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "PRESENCE_SHORT_WITHOUT_RETRY") {
				t.Fatalf("short initialization write: %s %v", out, err)
			}
		})
	}
}
