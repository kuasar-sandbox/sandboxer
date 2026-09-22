package vhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/kuasar-sandbox/accelerator/pkg/readerr"
	"github.com/kuasar-sandbox/sandboxer/internal/readretry"
)

type recordedBase struct {
	*bytes.Reader
	calls [][2]int64
}

func (r *recordedBase) ReadAt(p []byte, off int64) (int, error) {
	r.calls = append(r.calls, [2]int64{off, int64(len(p))})
	return r.Reader.ReadAt(p, off)
}
func (*recordedBase) Close() error { return nil }

func TestCOWBaseRangeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name               string
		size, base, off, n int
		upper              []int
		calls              int
	}{
		{"aligned", 1 << 20, 1 << 20, 0, 1 << 20, nil, 1},
		{"unaligned", 1 << 20, 1 << 20, 513, (1 << 20) - 1027, nil, 1},
		{"upper-and-eof", 8 * 4096, 6*4096 + 731, 513, 7*4096 + 83, []int{2, 4}, 3},
		{"bounded", 2<<20 + 8192, 2<<20 + 8192, 511, 2<<20 + 7000, nil, 3},
		{"past-base", 8 * 4096, 4096, 8192, 3 * 4096, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := patternedBytes(tc.base)
			base := &recordedBase{Reader: bytes.NewReader(data)}
			cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), base, DiffInit{CreateSize: int64(tc.size)})
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			want := make([]byte, tc.size)
			copy(want, data)
			for _, block := range tc.upper {
				page := bytes.Repeat([]byte{0xb7}, cowBlockSize)
				if _, err := cow.WriteAt(page, int64(block*cowBlockSize)); err != nil {
					t.Fatal(err)
				}
				copy(want[block*cowBlockSize:], page)
			}
			got := bytes.Repeat([]byte{0xdd}, tc.n)
			if n, err := cow.ReadAt(got, int64(tc.off)); n != len(got) || err != nil {
				t.Fatalf("read = %d, %v", n, err)
			}
			if !bytes.Equal(got, want[tc.off:tc.off+tc.n]) {
				t.Fatal("base/upper/EOF contents differ")
			}
			if len(base.calls) != tc.calls {
				t.Fatalf("base calls = %d, want %d", len(base.calls), tc.calls)
			}
			for _, call := range base.calls {
				if call[0] < int64(tc.off) || call[0]+call[1] > int64(tc.off+tc.n) || call[1] > maxDiffScratchSize {
					t.Fatalf("read outside requested/bounded range: %v", call)
				}
			}
		})
	}
}

func TestCOWBaseRangeReadRecovery(t *testing.T) {
	for _, mode := range []string{"retry", "terminal-eof", "permanent-eof", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			base := NewStreamReader(context.Background(), recoveryStream{read: func(ctx context.Context, p []byte) (int, error) {
				calls++
				switch mode {
				case "terminal-eof":
					return 0, readretry.Terminal(io.EOF)
				case "permanent-eof":
					return 0, readerr.Mark(io.EOF, false)
				case "cancel":
					cancel()
					<-ctx.Done()
					return 0, ctx.Err()
				case "retry":
					if calls == 1 {
						return 0, io.ErrClosedPipe
					}
				}
				for i := range p {
					p[i] = 0x9b
				}
				return len(p), io.EOF
			}}, maxDiffScratchSize)
			cow, err := OpenBlockCOW(filepath.Join(t.TempDir(), "diff"), base, DiffInit{CreateSize: maxDiffScratchSize})
			if err != nil {
				t.Fatal(err)
			}
			defer cow.Close()
			buf := make([]byte, maxDiffScratchSize)
			n, err := cow.readAt(ctx, buf, 0)
			if mode == "retry" {
				if n != len(buf) || err != nil || !bytes.Equal(buf, bytes.Repeat([]byte{0x9b}, len(buf))) {
					t.Fatalf("recovered read = %d, %v", n, err)
				}
				if calls != 2 {
					t.Fatalf("attempts = %d, want 2", calls)
				}
			} else if n != 0 || !readretry.IsTerminal(err) || calls != 1 {
				t.Fatalf("failed read = %d, %v, attempts = %d", n, err, calls)
			} else if mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}
