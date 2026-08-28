package restore

import (
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
			_, err := rewriteConfigPaths([]byte(tt.config), pathRewrite{
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
