package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRewriteConfigPathsRequiresExactDiskTopology(t *testing.T) {
	tests := []struct {
		name         string
		config       string
		diskSocks    []string
		diskReadOnly []bool
		wantErr      string
	}{
		{
			name:         "exact overlay",
			config:       `{"pmem":[{"file":"/old/runtime.bundle"}],"disks":[{"vhost_socket":"old0","readonly":true},{"vhost_socket":"old1","readonly":false}]}`,
			diskSocks:    []string{"new0", "new1"},
			diskReadOnly: []bool{true, false},
		},
		{
			name:         "sandbox has surplus device",
			config:       `{"disks":[{"vhost_socket":"old0"}]}`,
			diskSocks:    []string{"new0", "new1"},
			diskReadOnly: []bool{false, false},
			wantErr:      "disk topology mismatch",
		},
		{
			name:         "snapshot has surplus device",
			config:       `{"disks":[{"vhost_socket":"old0"},{"vhost_socket":"old1"}]}`,
			diskSocks:    []string{"new0"},
			diskReadOnly: []bool{false},
			wantErr:      "disk topology mismatch",
		},
		{
			name:         "non object device",
			config:       `{"disks":[null]}`,
			diskSocks:    []string{"new0"},
			diskReadOnly: []bool{false},
			wantErr:      "disks[0]",
		},
		{
			name:         "same count different roles",
			config:       `{"disks":[{"vhost_socket":"old0","readonly":true},{"vhost_socket":"old1"}]}`,
			diskSocks:    []string{"new0", "new1"},
			diskReadOnly: []bool{false, false},
			wantErr:      "readonly=true conflicts with Sandbox E readonly=false",
		},
		{
			name:         "malformed readonly",
			config:       `{"disks":[{"vhost_socket":"old0","readonly":"false"}]}`,
			diskSocks:    []string{"new0"},
			diskReadOnly: []bool{false},
			wantErr:      "readonly must be a boolean",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rewriteConfigPaths([]byte(tt.config), pathRewrite{
				DiskSocks: tt.diskSocks, DiskReadOnly: tt.diskReadOnly, RuntimePath: "/new/runtime.bundle",
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("rewriteConfigPaths error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRewriteConfigPathsRuntimePmemValidation(t *testing.T) {
	for _, field := range []string{
		``, `,"pmem":null`, `,"pmem":{}`, `,"pmem":[]`,
		`,"pmem":[{"file":"old"},{"file":"other"}]`,
		`,"pmem":[null]`, `,"pmem":["old"]`, `,"pmem":[{}]`,
		`,"pmem":[{"file":null}]`, `,"pmem":[{"file":7}]`, `,"pmem":[{"file":""}]`,
	} {
		_, err := rewriteConfigPaths([]byte(`{"disks":[]`+field+`}`), pathRewrite{RuntimePath: "/new/runtime.bundle"})
		if err == nil || !strings.Contains(err.Error(), "pmem") {
			t.Fatalf("invalid pmem %q: %v", field, err)
		}
	}
	for _, path := range []string{"", "relative/runtime.bundle"} {
		_, err := rewriteConfigPaths([]byte(`{"disks":[],"pmem":[{"file":"old"}]}`), pathRewrite{RuntimePath: path})
		if err == nil || !strings.Contains(err.Error(), "runtime path must be absolute") {
			t.Fatalf("invalid runtime path %q: %v", path, err)
		}
	}
}

func TestRewriteConfigPathsUsesSelectedRuntime(t *testing.T) {
	for _, primaryName := range []string{"runtime-v1.bundle", "runtime-v2.bundle"} {
		for _, size := range []string{``, `,"size":2097152`} {
			t.Run(primaryName+size, func(t *testing.T) {
				dir := t.TempDir()
				digest := strings.Repeat("a", 64)
				selected := filepath.Join(dir, "runtime-v1.bundle")
				primary := filepath.Join(dir, primaryName)
				writeRestoreRuntime(t, selected, digest)
				if primary != selected {
					writeRestoreRuntime(t, primary, strings.Repeat("b", 64))
				}
				path, err := resolveRestoreRuntime(context.Background(), "file://"+primary, "file://runtime-v1.bundle@digest:"+digest)
				if err != nil {
					t.Fatal(err)
				}
				input := []byte(`{"disks":[],"pmem":[{"file":"/old/unavailable/runtime-v1.bundle","id":"_pmem0","discard_writes":true,"iommu":false,"pci_segment":0` + size + `}],"payload":{"kernel":"unchanged"},"future":{"field":[1,true,"text"]}}`)
				before := bytes.Clone(input)
				output, err := rewriteConfigPaths(input, pathRewrite{RuntimePath: path})
				if err != nil {
					t.Fatal(err)
				}
				var want, got map[string]any
				if err := json.Unmarshal(input, &want); err != nil {
					t.Fatal(err)
				}
				want["pmem"].([]any)[0].(map[string]any)["file"] = selected
				if err := json.Unmarshal(output, &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("changed fields beyond selected pmem file: got %s", output)
				}
				if !bytes.Equal(input, before) {
					t.Fatal("rewrote original snapshot config")
				}
			})
		}
	}
}
