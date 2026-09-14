package usagereader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/ctl"
	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
	"golang.org/x/sys/unix"
)

func savedFixture(t testing.TB) (Options, usage.Record, []byte) {
	t.Helper()
	record := usage.Record{Sequence: 1, SavedUTC: 99, Snapshot: usage.Snapshot{
		SandboxID: "exact-sid", RunEpoch: "native-epoch", StartedUTC: 1, SampleInterval: 1e9, FlushInterval: 5e9,
		Counters: []usage.Counter{{Name: "guest.cpu", Source: "boot/pid/start", KnownTotal: usage.Uint128{Hi: 4, Lo: 9007199254740993}, LastRaw: 456, Hertz: 100, SourceKnown: true, Complete: true, Status: usage.OK}},
		Gauges:   []usage.Gauge{{Name: "guest.memory", Source: "ram", IntegralTotal: usage.Uint128{Hi: 8, Lo: 9007199254740993}, SpanTotal: 10, CoveredTotal: 9, Status: usage.Missing}},
	}}
	raw, err := usage.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	options := Options{SandboxID: "exact-sid", ControlSocket: filepath.Join(dir, "ctl.sock"), File: filepath.Join(dir, "exact-sid.usage"), Limit: 10}
	if err := os.WriteFile(options.File, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return options, record, raw
}

func BenchmarkSavedRead(b *testing.B) {
	options, _, _ := savedFixture(b)
	options.Offline = true
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Read(context.Background(), options); err != nil {
			b.Fatal(err)
		}
	}
}

func TestOfflinePreservesPrecisionTailAndCursor(t *testing.T) {
	options, record, prefix := savedFixture(t)
	second := record
	second.Sequence++
	tail, err := usage.EncodeRecord(second)
	if err != nil {
		t.Fatal(err)
	}
	bytesOnDisk := append(append([]byte{}, prefix...), tail[:len(tail)/2]...)
	if err := os.WriteFile(options.File, bytesOnDisk, 0600); err != nil {
		t.Fatal(err)
	}
	for _, offline := range []bool{false, true} {
		options.Offline = offline
		raw, err := Read(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		var view usage.View
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		if view.Live != nil || !view.UnknownTail || view.SavedEnd != int64(len(prefix)) || !reflect.DeepEqual(view.Saved, &record) {
			t.Fatalf("lossy or fabricated view: %s", raw)
		}
	}
	options.History, options.Limit = true, 1
	raw, err := Read(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Records []usage.Record `json:"records"`
		Next    int64          `json:"next_cursor,string"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || !reflect.DeepEqual(page.Records, []usage.Record{record}) || page.Next != int64(len(prefix)) {
		t.Fatalf("history: %s %v", raw, err)
	}
	options.Cursor = page.Next
	if _, err := Read(context.Background(), options); err != nil {
		t.Fatal("valid empty final page", err)
	}
	options.Cursor--
	if _, err := Read(context.Background(), options); err == nil {
		t.Fatal("non-boundary cursor accepted")
	}
	options.Cursor, options.SandboxID = 0, "stable-alias"
	if _, err := Read(context.Background(), options); err == nil {
		t.Fatal("different SandboxID accepted")
	}
	after, err := os.ReadFile(options.File)
	if err != nil || !bytes.Equal(after, bytesOnDisk) {
		t.Fatal("reader changed native file", err)
	}
}

func startOwner(t *testing.T, socket string, handler func(ctl.Request) (ctl.Response, error)) {
	t.Helper()
	server := &ctl.Server{Path: socket, UsageHandler: handler, SnapshotHandler: func(ctl.Request) (ctl.Response, error) {
		t.Error("usage triggered snapshot")
		return ctl.Response{}, errors.New("unexpected snapshot")
	}}
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func TestOwnerLiveSavedHistoryAndWriterLock(t *testing.T) {
	options, record, before := savedFixture(t)
	manager, err := usage.Open(filepath.Dir(options.File), options.SandboxID, "next-native-epoch", time.Now(), time.Second, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		manager.Close(ctx, time.Now())
	})
	want := manager.View()
	startOwner(t, options.ControlSocket, func(request ctl.Request) (ctl.Response, error) {
		var value any = manager.View()
		if request.UsageHistory {
			records, next, err := manager.History(request.UsageCursor, request.UsageLimit)
			if err != nil {
				return ctl.Response{}, err
			}
			value = struct {
				Records []usage.Record `json:"records"`
				Next    int64          `json:"next_cursor,string"`
			}{records, next}
		}
		raw, err := json.Marshal(value)
		return ctl.Response{SandboxID: options.SandboxID, Usage: raw}, err
	})
	for _, saved := range []bool{false, true} {
		options.Saved = saved
		raw, err := Read(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		var view usage.View
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		if saved {
			want.Live = nil
		}
		if !reflect.DeepEqual(view, want) || !reflect.DeepEqual(view.Saved, &record) {
			t.Fatalf("owner view changed: %s", raw)
		}
	}
	options.History, options.Limit = true, 1
	if _, err := Read(context.Background(), options); err != nil {
		t.Fatal("online history", err)
	}
	options.Offline = true
	if _, err := Read(context.Background(), options); err == nil || !strings.Contains(err.Error(), "live writer") {
		t.Fatal("offline read bypassed active writer", err)
	}
	after, err := os.ReadFile(options.File)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(manager.View().Saved, &record) {
		t.Fatal("reading sampled or saved native usage", err)
	}
}

func TestOnlineErrorsNeverFallback(t *testing.T) {
	for _, kind := range []string{"owner-error", "bad-json", "null-snapshot", "null-history", "wrong-sid"} {
		t.Run(kind, func(t *testing.T) {
			options, _, _ := savedFixture(t)
			options.History = kind == "null-history"
			startOwner(t, options.ControlSocket, func(ctl.Request) (ctl.Response, error) {
				switch kind {
				case "owner-error":
					return ctl.Response{}, errors.New("owner failed")
				case "bad-json":
					return ctl.Response{SandboxID: options.SandboxID, Usage: json.RawMessage(`"wrong shape"`)}, nil
				case "null-snapshot", "null-history":
					return ctl.Response{SandboxID: options.SandboxID, Usage: json.RawMessage(`null`)}, nil
				default:
					return ctl.Response{SandboxID: options.SandboxID, Usage: json.RawMessage(`{"live":{"sandbox_id":"another"}}`)}, nil
				}
			})
			if _, err := Read(context.Background(), options); err == nil {
				t.Fatal("online error replaced with readable saved file")
			}
		})
	}
}

func TestEmptyOwnerResponseStillRequiresIdentity(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, ownerID := range []string{"", "different", "exact-sid"} {
			t.Run(fmt.Sprintf("history=%v/owner=%s", history, ownerID), func(t *testing.T) {
				options, _, _ := savedFixture(t)
				options.History = history
				startOwner(t, options.ControlSocket, func(ctl.Request) (ctl.Response, error) {
					body := json.RawMessage(`{"enabled":false,"saved_end":"0","saving":false,"unknown_tail":false}`)
					if history {
						body = json.RawMessage(`{"records":[],"next_cursor":"0"}`)
					}
					return ctl.Response{SandboxID: ownerID, Usage: body}, nil
				})
				_, err := Read(context.Background(), options)
				if (err == nil) != (ownerID == options.SandboxID) {
					t.Fatalf("empty owner's identity check: %v", err)
				}
			})
		}
	}
}

func TestOwnerCursorValidationDoesNotFallBack(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, saved := range []bool{false, true} {
			for _, cursor := range []string{"missing", "null", "bad", "-1", "0", "7", "8"} {
				t.Run(fmt.Sprintf("history=%v/record=%v/cursor=%s", history, saved, cursor), func(t *testing.T) {
					options, record, _ := savedFixture(t)
					options.History = history
					body := map[string]any{}
					field := "saved_end"
					if history {
						options.Cursor = 7
						field = "next_cursor"
						body["records"] = []usage.Record{}
						if saved {
							body["records"] = []usage.Record{record}
						}
					} else if saved {
						body["saved"] = record
					}
					if cursor != "missing" {
						if cursor == "null" {
							body[field] = nil
						} else {
							body[field] = cursor
						}
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					startOwner(t, options.ControlSocket, func(ctl.Request) (ctl.Response, error) {
						return ctl.Response{SandboxID: options.SandboxID, Usage: raw}, nil
					})
					valid := saved && (cursor == "7" || cursor == "8") || !saved && (cursor == "0" || cursor == "missing" || cursor == "null")
					if history {
						valid = saved && cursor == "8" || !saved && cursor == "7"
					}
					got, err := Read(context.Background(), options)
					if (err == nil) != valid || valid && !bytes.Equal(got, raw) {
						t.Fatalf("owner cursor validation: valid=%v, got=%s, err=%v", valid, got, err)
					}
				})
			}
		}
	}
}

func TestOwnerCancellationAfterTransport(t *testing.T) {
	for _, view := range []string{"current", "saved", "history"} {
		t.Run(view, func(t *testing.T) {
			options, record, _ := savedFixture(t)
			options.Saved, options.History = view == "saved", view == "history"
			// A bounded but large legal JSON page exercises the second decode,
			// beyond ReadUsage's completed connection/deadline checks.
			record.Snapshot.Counters[0].Source = strings.Repeat("x", 900000)
			var value any = usage.View{Saved: &record, SavedEnd: 1}
			if options.History {
				value = map[string]any{"records": []usage.Record{record}, "next_cursor": "1"}
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			startOwner(t, options.ControlSocket, func(ctl.Request) (ctl.Response, error) {
				return ctl.Response{SandboxID: options.SandboxID, Usage: raw}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			response, connected, err := ctl.ReadUsage(ctx, options.ControlSocket, options.History, options.Cursor, options.Limit)
			if err != nil || !connected {
				t.Fatal("owner transport", connected, err)
			}
			if _, err := decodeOwner(ctx, options, response); err != nil {
				t.Fatal("valid owner page", err)
			}
			cancel() // No transport remains to observe this cancellation.
			if body, err := decodeOwner(ctx, options, response); len(body) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("post-transport processing returned canceled data: bytes=%d err=%v", len(body), err)
			}
		})
	}
}

func TestCanceledOfflineReadsRetainBoundedSlotsAndFileLocks(t *testing.T) {
	options, _, raw := savedFixture(t)
	release := make(chan struct{})
	var workers sync.WaitGroup
	t.Cleanup(func() {
		close(release)
		workers.Wait()
	})
	for range cap(offlineSlots) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, returned := make(chan error, 1), make(chan error, 1)
		workers.Add(1)
		go func() {
			_, err := waitOffline(ctx, func() (json.RawMessage, error) {
				defer workers.Done()
				f, err := os.Open(options.File)
				if err != nil {
					started <- err
					return nil, err
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
					started <- err
					return nil, err
				}
				started <- nil
				// Model a file syscall that does not honor context cancellation.
				// Its real shared lock must remain owned until execution ends.
				<-release
				_, err = usage.Recover(f, int64(len(raw)), options.SandboxID)
				return nil, err
			})
			returned <- err
		}()
		if err := <-started; err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case err := <-returned:
			if !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
		case <-time.After(time.Second):
			t.Fatal("caller remained blocked in file I/O")
		}
	}
	f, err := os.Open(options.File)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatal("canceled read released a lock before I/O ended", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := waitOffline(ctx, func() (json.RawMessage, error) {
		t.Error("blocked reads exceeded the execution bound")
		return nil, nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("exhausted reader slots ignored deadline", err)
	}
}

func TestOfflineFIFOIsRejectedWithoutWaitingForWriter(t *testing.T) {
	options, _, _ := savedFixture(t)
	options.File, options.Offline = filepath.Join(t.TempDir(), "blocked.usage"), true
	if err := unix.Mkfifo(options.File, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := Read(ctx, options); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not regular") {
			t.Fatal("FIFO not rejected", err)
		}
	case <-time.After(time.Second):
		// Release a regressed blocking open before failing, so the test cannot
		// retain a goroutine or let cleanup hide the missing timeout guarantee.
		f, _ := os.OpenFile(options.File, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if f != nil {
			_ = f.Close()
		}
		t.Fatal("offline open waited for a FIFO writer")
	}
}

func TestRefusedSocketFallbackAndCancellation(t *testing.T) {
	options, _, _ := savedFixture(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: options.ControlSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	_ = listener.Close()
	if _, err := Read(context.Background(), options); err != nil {
		t.Fatal("refused socket did not allow saved file", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, options); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled query read saved data", err)
	}
}
