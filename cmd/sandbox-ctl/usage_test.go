package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/usage"
)

func TestUsageOfflineIdentityAndLiveWriter(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "instance")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	m, err := usage.Open(dir, "logical", "epoch", time.Now(), time.Second, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"--sandbox-id", "logical", "--path-id", "instance", "--base-root", root, "--offline"}
	if err := runUsage(args, &out); err == nil {
		t.Fatal("offline read bypassed live writer")
	}
	_ = m.Counter("guest.cpu", "process", 15, 100, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	m.Close(ctx, time.Now())
	cancel()
	if err := runUsage(args, &out); err != nil {
		t.Fatal(err)
	}
	var v usage.View
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Live != nil || v.Saved == nil || v.Saved.Snapshot.SandboxID != "logical" {
		t.Fatalf("%s", out.Bytes())
	}
	if len(v.Saved.Snapshot.Counters) != 1 || !v.Saved.Snapshot.Counters[0].Complete {
		t.Fatal("offline query changed the saved prefix's completeness")
	}
	before, err := os.ReadFile(filepath.Join(dir, "logical.usage"))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runUsage(append(args, "--history", "--limit", "1"), &out); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Records []usage.Record `json:"records"`
		Next    int64          `json:"next_cursor,string"`
	}
	if err := json.Unmarshal(out.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Next != int64(len(before)) {
		t.Fatalf("%+v", page)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "logical.usage"))
	if !bytes.Equal(before, after) {
		t.Fatal("query modified usage file")
	}
	out.Reset()
	if err := runUsage([]string{"--file", filepath.Join(dir, "logical.usage"), "--sandbox-id", "wrong"}, &out); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	if err := runUsage(append(args, "--json=false"), &out); err == nil {
		t.Fatal("unsupported output format")
	}
	// Only a new accumulating owner's live baseline is downgraded. Its adopted
	// saved record, and the bytes returned by an offline read, remain unchanged.
	next, err := usage.Open(dir, "logical", "next", time.Now(), time.Second, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer next.Close(ctx, time.Now())
	got := next.View()
	if got.Saved == nil || got.Live == nil || len(got.Live.Counters) != 1 {
		t.Fatalf("reopen lost the saved/live baseline: %+v", got)
	}
	encoded, err := usage.EncodeRecord(*got.Saved)
	if err != nil || !bytes.Equal(before, encoded) {
		t.Fatalf("reopen changed the adopted saved record: %v", err)
	}
	if got.Live.Counters[0].Complete || got.Live.Counters[0].KnownTotal != v.Saved.Snapshot.Counters[0].KnownTotal {
		t.Fatalf("incorrect live history baseline: %+v", got.Live.Counters[0])
	}
	after, err = os.ReadFile(filepath.Join(dir, "logical.usage"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("adopting saved state modified the file: %v", err)
	}
}
