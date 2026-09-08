package journalio

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestWriterCRLFAtFrameBoundary(t *testing.T) {
	for _, frames := range []int{1, 2} {
		for _, split := range []int{0, MaxLine - 1, MaxLine, frames*MaxLine - 1, frames * MaxLine} {
			for _, failed := range []bool{false, true} {
				name := fmt.Sprintf("frames=%d/split=%d/failed=%t", frames, split, failed)
				t.Run(name, func(t *testing.T) {
					var messages []string
					var fallback bytes.Buffer
					w, err := newWriter(Target{Tag: "app"}, &fallback, "", func(message string, _ map[string]string) error {
						messages = append(messages, message)
						if failed {
							return errors.New("journal unavailable")
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					// CR is the final byte of a full frame. LF may arrive in the
					// same Write or a later call, but native messages must match.
					input := strings.Repeat("x", frames*MaxLine-1) + "\r\nnext\n"
					for _, fragment := range []string{input[:split], input[split:]} {
						if n, err := w.Write([]byte(fragment)); n != len(fragment) || err != nil {
							t.Fatalf("Write = %d, %v", n, err)
						}
						if len(w.buf) > MaxLine || cap(w.buf) > MaxLine {
							t.Fatalf("unbounded buffer: len=%d cap=%d", len(w.buf), cap(w.buf))
						}
					}
					_ = w.Close()
					_ = w.Close()
					var want []string
					for i := 1; i < frames; i++ {
						want = append(want, strings.Repeat("x", MaxLine))
					}
					want = append(want, strings.Repeat("x", MaxLine-1), "next")
					if !reflect.DeepEqual(messages, want) {
						t.Fatalf("incorrect normalized frames: got lengths %v, want %v", frameLengths(messages), frameLengths(want))
					}
					wantFallback := ""
					if failed {
						wantFallback = strings.Repeat(strings.Repeat("x", MaxLine)+"\n", frames-1) + strings.Repeat("x", MaxLine-1) + "\r\nnext\n"
					}
					if fallback.String() != wantFallback {
						t.Fatal("fallback did not preserve the CRLF line ending")
					}
				})
			}
		}
	}
}

func frameLengths(frames []string) []int {
	out := make([]int, len(frames))
	for i, frame := range frames {
		out[i] = len(frame)
	}
	return out
}

func TestWriterFullFrameKeepsDataCRAndEOFTail(t *testing.T) {
	first := strings.Repeat("x", MaxLine-1) + "\r"
	for _, suffix := range []string{"", "rest\n"} {
		t.Run(suffix, func(t *testing.T) {
			var messages []string
			w, err := newWriter(Target{Tag: "app"}, nil, "", func(message string, _ map[string]string) error {
				messages = append(messages, message)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(first))
			_, _ = w.Write(nil)
			_, _ = w.Write([]byte(suffix))
			_ = w.Close()
			want := []string{first}
			if suffix != "" {
				want = append(want, "rest")
			}
			if !reflect.DeepEqual(messages, want) {
				t.Fatal("a forced frame or EOF discarded a data CR")
			}
		})
	}
}
