package restore

import (
	"encoding/json"
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
			config:       `{"disks":[{"vhost_socket":"old0","readonly":true},{"vhost_socket":"old1","readonly":false}]}`,
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
			_, _, err := rewriteConfigPaths([]byte(tt.config), pathRewrite{
				DiskSocks: tt.diskSocks, DiskReadOnly: tt.diskReadOnly,
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

func TestRewriteConfigPaths_NetworkBackendTransitions(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		rewrite     pathRewrite
		wantID      string
		wantTap     string
		wantHasTap  bool
		wantHasFDs  bool
		wantFDCount int
	}{
		{
			name:       "tap(old) -> tap(new)",
			input:      `{"disks":[],"net":[{"id":"_net0","tap":"old-tap","mac":"02:00:00:00:80:01"}]}`,
			rewrite:    pathRewrite{TargetTap: "new-tap"},
			wantID:     "_net0",
			wantTap:    "new-tap",
			wantHasTap: true,
			wantHasFDs: false,
		},
		{
			name:        "tap -> tapfd",
			input:       `{"disks":[],"net":[{"id":"_net0","tap":"old-tap","mac":"02:00:00:00:80:01"}]}`,
			rewrite:     pathRewrite{IsTapFD: true},
			wantID:      "_net0",
			wantHasTap:  false,
			wantHasFDs:  true,
			wantFDCount: 1,
		},
		{
			name:       "tapfd -> tap",
			input:      `{"disks":[],"net":[{"id":"_net0","fds":[-1],"mac":"02:00:00:00:80:01"}]}`,
			rewrite:    pathRewrite{TargetTap: "new-tap"},
			wantID:     "_net0",
			wantTap:    "new-tap",
			wantHasTap: true,
			wantHasFDs: false,
		},
		{
			name:        "tapfd -> tapfd",
			input:       `{"disks":[],"net":[{"id":"_net0","fds":[-1],"mac":"02:00:00:00:80:01"}]}`,
			rewrite:     pathRewrite{IsTapFD: true},
			wantID:      "_net0",
			wantHasTap:  false,
			wantHasFDs:  true,
			wantFDCount: 1,
		},
		{
			name:       "non-default net device id preserved across transition",
			input:      `{"disks":[],"net":[{"id":"net1","tap":"old-tap","mac":"02:00:00:00:80:01"}]}`,
			rewrite:    pathRewrite{TargetTap: "fresh-tap"},
			wantID:     "net1",
			wantTap:    "fresh-tap",
			wantHasTap: true,
			wantHasFDs: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, id, err := rewriteConfigPaths([]byte(tt.input), tt.rewrite)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("device ID = %q, want %q", id, tt.wantID)
			}

			var res map[string]any
			if err := json.Unmarshal(out, &res); err != nil {
				t.Fatalf("failed to unmarshal result: %v", err)
			}
			nets, ok := res["net"].([]any)
			if !ok || len(nets) != 1 {
				t.Fatalf("expected 1 net device, got %v", res["net"])
			}
			nm, ok := nets[0].(map[string]any)
			if !ok {
				t.Fatalf("net[0] is not an object: %v", nets[0])
			}

			tapVal, hasTap := nm["tap"]
			if hasTap != tt.wantHasTap {
				t.Errorf("hasTap = %v, want %v", hasTap, tt.wantHasTap)
			}
			if tt.wantHasTap && tapVal != tt.wantTap {
				t.Errorf("tap = %v, want %v", tapVal, tt.wantTap)
			}

			fdsVal, hasFDs := nm["fds"]
			if hasFDs != tt.wantHasFDs {
				t.Errorf("hasFDs = %v, want %v", hasFDs, tt.wantHasFDs)
			}
			if tt.wantHasFDs {
				fdsSlice, ok := fdsVal.([]any)
				if !ok {
					t.Fatalf("fds is not an array: %v", fdsVal)
				}
				if len(fdsSlice) != tt.wantFDCount {
					t.Errorf("fds len = %d, want %d", len(fdsSlice), tt.wantFDCount)
				}
				for i, v := range fdsSlice {
					// JSON numbers unmarshal as float64 when unmarshaled into any
					if num, ok := v.(float64); !ok || int(num) != -1 {
						t.Errorf("fds[%d] = %v, want -1", i, v)
					}
				}
			}

			// Verify MAC is preserved
			if nm["mac"] != "02:00:00:00:80:01" {
				t.Errorf("mac = %v, want 02:00:00:00:80:01", nm["mac"])
			}
		})
	}
}

func TestRewriteConfigPaths_NetworkEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		rewrite pathRewrite
		wantErr string
		wantID  string
	}{
		{
			name:    "no-NIC null without network target succeeds",
			input:   `{"disks":[],"net":null}`,
			rewrite: pathRewrite{},
			wantID:  "",
		},
		{
			name:    "no-NIC empty array without network target succeeds",
			input:   `{"disks":[],"net":[]}`,
			rewrite: pathRewrite{},
			wantID:  "",
		},
		{
			name:    "no-NIC omitted without network target succeeds",
			input:   `{"disks":[]}`,
			rewrite: pathRewrite{},
			wantID:  "",
		},
		{
			name:    "no-NIC null with target tap fails",
			input:   `{"disks":[],"net":null}`,
			rewrite: pathRewrite{TargetTap: "tap0"},
			wantErr: "cannot rebind network to snapshot with no network device",
		},
		{
			name:    "no-NIC empty array with target tapfd fails",
			input:   `{"disks":[],"net":[]}`,
			rewrite: pathRewrite{IsTapFD: true},
			wantErr: "cannot rebind network to snapshot with no network device",
		},
		{
			name:    "no-NIC omitted with target tap fails",
			input:   `{"disks":[]}`,
			rewrite: pathRewrite{TargetTap: "tap0"},
			wantErr: "cannot rebind network to snapshot with no network device",
		},
		{
			name:    "malformed net string",
			input:   `{"disks":[],"net":"invalid"}`,
			rewrite: pathRewrite{},
			wantErr: "config.json.net must be an array or null",
		},
		{
			name:    "malformed net object",
			input:   `{"disks":[],"net":{"id":"_net0"}}`,
			rewrite: pathRewrite{},
			wantErr: "config.json.net must be an array or null",
		},
		{
			name:    "malformed net[0] null",
			input:   `{"disks":[],"net":[null]}`,
			rewrite: pathRewrite{},
			wantErr: "config.json.net[0] must be an object",
		},
		{
			name:    "malformed net[0] non-object",
			input:   `{"disks":[],"net":["not-an-object"]}`,
			rewrite: pathRewrite{},
			wantErr: "config.json.net[0] must be an object",
		},
		{
			name:    "multiple net devices rejected",
			input:   `{"disks":[],"net":[{"id":"_net0"},{"id":"_net1"}]}`,
			rewrite: pathRewrite{},
			wantErr: "expected at most one device",
		},
		{
			name:    "missing ID for tapfd target fails closed",
			input:   `{"disks":[],"net":[{"tap":"old-tap"}]}`,
			rewrite: pathRewrite{IsTapFD: true},
			wantErr: "captured network device has missing or empty id for fd-backed restore",
		},
		{
			name:    "empty ID string for tapfd target fails closed",
			input:   `{"disks":[],"net":[{"id":"","tap":"old-tap"}]}`,
			rewrite: pathRewrite{IsTapFD: true},
			wantErr: "captured network device has missing or empty id for fd-backed restore",
		},
		{
			name:    "conflicting target options rejected",
			input:   `{"disks":[],"net":[{"id":"_net0","tap":"old-tap"}]}`,
			rewrite: pathRewrite{TargetTap: "tap0", IsTapFD: true},
			wantErr: "cannot specify both TargetTap and IsTapFD",
		},
		{
			name:    "multi-queue device with multiple fds fails closed for tapfd target",
			input:   `{"disks":[],"net":[{"id":"_net0","fds":[-1,-1],"mac":"02:00:00:00:80:01"}]}`,
			rewrite: pathRewrite{IsTapFD: true},
			wantErr: "multi-queue net device (2 queue fds) cannot be restored with single-queue tapfd",
		},
		{
			name:    "multi-queue device with num_queues > 2 fails closed for tapfd target",
			input:   `{"disks":[],"net":[{"id":"_net0","num_queues":4,"mac":"02:00:00:00:80:01"}]}`,
			rewrite: pathRewrite{IsTapFD: true},
			wantErr: "multi-queue net device (4 virtqueues) cannot be restored with single-queue tapfd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, id, err := rewriteConfigPaths([]byte(tt.input), tt.rewrite)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != tt.wantID {
				t.Errorf("device ID = %q, want %q", id, tt.wantID)
			}
		})
	}
}

func TestRewriteConfigPaths_NetworkFieldPreservation(t *testing.T) {
	inputJSON := `{
		"disks": [],
		"net": [{
			"id": "my_net_0",
			"tap": "original-tap",
			"mac": "02:42:ac:11:00:02",
			"host_mac": "02:42:ac:11:00:01",
			"mtu": 1500,
			"num_queues": 2,
			"queue_size": 256,
			"offload_tso": false,
			"offload_ufo": false,
			"offload_csum": true,
			"iommu": true,
			"pci_segment": 2
		}]
	}`

	verifyPreservedFields := func(t *testing.T, nm map[string]any) {
		t.Helper()
		expected := map[string]any{
			"id":           "my_net_0",
			"mac":          "02:42:ac:11:00:02",
			"host_mac":     "02:42:ac:11:00:01",
			"mtu":          float64(1500),
			"num_queues":   float64(2),
			"queue_size":   float64(256),
			"offload_tso":  false,
			"offload_ufo":  false,
			"offload_csum": true,
			"iommu":        true,
			"pci_segment":  float64(2),
		}
		for k, want := range expected {
			got, ok := nm[k]
			if !ok {
				t.Errorf("missing preserved field %s", k)
				continue
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("field %s = %v (%T), want %v (%T)", k, got, got, want, want)
			}
		}
	}

	t.Run("preservation on tap target", func(t *testing.T) {
		out, id, err := rewriteConfigPaths([]byte(inputJSON), pathRewrite{TargetTap: "target-tap"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "my_net_0" {
			t.Errorf("ID = %q, want my_net_0", id)
		}

		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatal(err)
		}
		nm := res["net"].([]any)[0].(map[string]any)
		if nm["tap"] != "target-tap" {
			t.Errorf("tap = %v, want target-tap", nm["tap"])
		}
		if _, hasFDs := nm["fds"]; hasFDs {
			t.Errorf("fds should not be present on tap target")
		}
		verifyPreservedFields(t, nm)
	})

	t.Run("preservation on tapfd target", func(t *testing.T) {
		out, id, err := rewriteConfigPaths([]byte(inputJSON), pathRewrite{IsTapFD: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "my_net_0" {
			t.Errorf("ID = %q, want my_net_0", id)
		}

		var res map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatal(err)
		}
		nm := res["net"].([]any)[0].(map[string]any)
		if _, hasTap := nm["tap"]; hasTap {
			t.Errorf("tap should not be present on tapfd target")
		}
		fds, ok := nm["fds"].([]any)
		if !ok || len(fds) != 1 {
			t.Fatalf("fds = %v, want [-1]", nm["fds"])
		}
		verifyPreservedFields(t, nm)
	})
}
