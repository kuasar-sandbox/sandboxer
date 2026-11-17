package config

import (
	"testing"
	"time"
)

func TestDiffCOWCacheIdleAfterConfig(t *testing.T) {
	for _, tc := range []struct {
		input   string
		want    time.Duration
		invalid bool
	}{
		{input: "{}", want: 10 * time.Second},
		{input: "resources: {diff_cow: {cache_idle_after: 30s}}", want: 30 * time.Second},
		{input: "resources: {diff_cow: {cache_idle_after: 250ms}}", want: 250 * time.Millisecond},
		{input: "resources: {diff_cow: {cache_idle_after: 0s}}", invalid: true},
		{input: "resources: {diff_cow: {cache_idle_after: -1s}}", invalid: true},
		{input: "resources: {diff_cow: {cache_idle_after: banana}}", invalid: true},
		{input: "resources: {diff_cow: {cache_idle_after: null}}", invalid: true},
		{input: "resources: {diff_cow: {cache_age: 10s}}", invalid: true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			cfg, _, err := LoadConfigBytesWithPresence([]byte(tc.input))
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid idle policy accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			before := cfg.Resources.DiffCOW
			got, err := before.CacheIdleDuration()
			if err != nil || got != tc.want {
				t.Fatalf("duration=%v, err=%v, want=%v", got, err, tc.want)
			}
			resolved, err := before.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			got, err = resolved.CacheIdleDuration()
			if err != nil || got != tc.want {
				t.Fatalf("resolved duration=%v, err=%v", got, err)
			}
			if before != cfg.Resources.DiffCOW {
				t.Fatal("source was mutated")
			}
		})
	}
}

func TestCacheIdleAfterHostMerge(t *testing.T) {
	paths := []string{
		writeYAML(t, "resources: {diff_cow: {cache_size: 64MiB, cache_idle_after: 20s}}"),
		writeYAML(t, "resources: {diff_cow: {max_dirty_size: 32MiB}}"),
	}
	cfg, presence, err := LoadMergedWithPresence(paths)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resources.DiffCOW.CacheIdleAfter != "20s" || !presence.Has("resources.diff_cow.cache_idle_after") {
		t.Fatalf("policy lost across host overrides: %+v", cfg.Resources.DiffCOW)
	}
}
