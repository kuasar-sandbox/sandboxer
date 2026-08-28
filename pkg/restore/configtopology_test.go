package restore

import (
	"context"
	"strings"
	"testing"

	"github.com/kuasar-sandbox/sandboxer/pkg/config"
)

func TestSnapshotConfigHasNetwork(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		want    bool
		wantErr string
	}{
		{name: "missing", config: `{}`},
		{name: "null", config: `{"net":null}`},
		{name: "empty", config: `{"net":[]}`},
		{name: "one", config: `{"net":[{"id":"_net0"}]}`, want: true},
		{name: "null device", config: `{"net":[null]}`, wantErr: "net[0]: expected JSON object"},
		{name: "string device", config: `{"net":["tap0"]}`, wantErr: "net[0]: expected JSON object"},
		{name: "array device", config: `{"net":[[]]}`, wantErr: "net[0]: expected JSON object"},
		{name: "multiple", config: `{"net":[{"id":"_net0"},{"id":"_net1"}]}`, wantErr: "expected at most one device"},
		{name: "object", config: `{"net":{}}`, wantErr: "expected array or null"},
		{name: "string", config: `{"net":"tap0"}`, wantErr: "expected array or null"},
		{name: "malformed", config: `{`, wantErr: "snapshot config.json"},
		{name: "null root", config: `null`, wantErr: "expected JSON object"},
		{name: "array root", config: `[]`, wantErr: "snapshot config.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := snapshotConfigHasNetwork([]byte(tt.config))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("snapshotConfigHasNetwork() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("snapshotConfigHasNetwork() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("snapshotConfigHasNetwork() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestValidateRestoreNetworkTopology(t *testing.T) {
	tests := []struct {
		name               string
		snapshotHasNetwork bool
		hostHasNetwork     bool
		wantErr            string
	}{
		{name: "no network remains absent"},
		{name: "network remains present", snapshotHasNetwork: true, hostHasNetwork: true},
		{
			name:               "cannot remove network",
			snapshotHasNetwork: true,
			wantErr:            "snapshot has a network device but restore config has no network source",
		},
		{
			name:           "cannot add network",
			hostHasNetwork: true,
			wantErr:        "snapshot has no network device but restore config provides a network source",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRestoreNetworkTopology(tt.snapshotHasNetwork, tt.hostHasNetwork)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateRestoreNetworkTopology() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateRestoreNetworkTopology() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSnapshotVCPUTopology(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		capacity int
		wantErr  string
	}{
		{
			name:     "exact",
			config:   `{"cpus":{"boot_vcpus":2,"max_vcpus":2}}`,
			capacity: 2,
		},
		{
			name:     "different boot count",
			config:   `{"cpus":{"boot_vcpus":1,"max_vcpus":1}}`,
			capacity: 2,
			wantErr:  "CH boot=1 max=1 Sandbox=2",
		},
		{
			name:     "different max count",
			config:   `{"cpus":{"boot_vcpus":2,"max_vcpus":4}}`,
			capacity: 2,
			wantErr:  "CH boot=2 max=4 Sandbox=2",
		},
		{name: "missing cpus", config: `{}`, capacity: 1, wantErr: "config.json.cpus is required"},
		{name: "null cpus", config: `{"cpus":null}`, capacity: 1, wantErr: "config.json.cpus must be an object"},
		{
			name:     "missing boot",
			config:   `{"cpus":{"max_vcpus":1}}`,
			capacity: 1,
			wantErr:  "boot_vcpus is required",
		},
		{
			name:     "missing max",
			config:   `{"cpus":{"boot_vcpus":1}}`,
			capacity: 1,
			wantErr:  "max_vcpus is required",
		},
		{
			name:     "fractional boot",
			config:   `{"cpus":{"boot_vcpus":1.5,"max_vcpus":2}}`,
			capacity: 2,
			wantErr:  "boot_vcpus",
		},
		{
			name:     "zero",
			config:   `{"cpus":{"boot_vcpus":0,"max_vcpus":0}}`,
			capacity: 1,
			wantErr:  "must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSnapshotVCPUTopology([]byte(tt.config), tt.capacity)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateSnapshotVCPUTopology() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateSnapshotVCPUTopology() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunValidatesHostNetworkBeforeOpeningSnapshot(t *testing.T) {
	cfg := &config.SandboxConfig{Network: config.NetworkConfig{IP: "192.0.2.1/24"}}
	_, err := Run(context.Background(), Options{
		SnapshotPath: "/does/not/exist.snapshot",
		HostCfg:      cfg,
	})
	if err == nil || !strings.Contains(err.Error(), "network.ip requires network.tap or network.tapfd") {
		t.Fatalf("Run() error = %v, want host network validation before snapshot access", err)
	}
}
