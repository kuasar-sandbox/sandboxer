package restore

import (
	"strings"
	"testing"
)

func TestRewriteConfigPathsRequiresExactDiskTopology(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		diskSocks []string
		wantErr   string
	}{
		{
			name:      "exact",
			config:    `{"disks":[{"vhost_socket":"old0"},{"vhost_socket":"old1"}]}`,
			diskSocks: []string{"new0", "new1"},
		},
		{
			name:      "sandbox has surplus device",
			config:    `{"disks":[{"vhost_socket":"old0"}]}`,
			diskSocks: []string{"new0", "new1"},
			wantErr:   "disk topology mismatch",
		},
		{
			name:      "snapshot has surplus device",
			config:    `{"disks":[{"vhost_socket":"old0"},{"vhost_socket":"old1"}]}`,
			diskSocks: []string{"new0"},
			wantErr:   "disk topology mismatch",
		},
		{
			name:      "non object device",
			config:    `{"disks":[null]}`,
			diskSocks: []string{"new0"},
			wantErr:   "disks[0]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rewriteConfigPaths([]byte(tt.config), pathRewrite{DiskSocks: tt.diskSocks})
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
