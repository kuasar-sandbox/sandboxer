package config

import (
	"testing"
)

func TestEffectiveNetwork(t *testing.T) {
	cases := []struct {
		name        string
		cfg         NetworkConfig
		mMAC, mIP   string
		wantMAC     string
		wantIPCIDR  string // "" → expect nil spec
		wantMTU     int
		wantNexthop string
	}{
		{
			name: "meta overrides, config mask preserved",
			cfg:  NetworkConfig{MAC: "aa:aa:aa:aa:aa:aa", IP: "169.254.1.1/31", MTU: 1500, Nexthop: "169.254.1.0"},
			mMAC: "02:00:00:00:80:01", mIP: "169.254.4.1",
			wantMAC: "02:00:00:00:80:01", wantIPCIDR: "169.254.4.1/31", wantMTU: 1500, wantNexthop: "169.254.1.0",
		},
		{
			name:    "no meta uses config as-is",
			cfg:     NetworkConfig{MAC: "aa:aa:aa:aa:aa:aa", IP: "10.0.0.5/24", MTU: 9000},
			wantMAC: "aa:aa:aa:aa:aa:aa", wantIPCIDR: "10.0.0.5/24", wantMTU: 9000,
		},
		{
			name:       "meta ip carrying a mask wins whole",
			cfg:        NetworkConfig{IP: "10.0.0.1/24"},
			mIP:        "192.168.0.9/30",
			wantIPCIDR: "192.168.0.9/30",
		},
		{
			name:       "bare meta ip with no config mask falls back to /32",
			mIP:        "169.254.7.7",
			wantIPCIDR: "169.254.7.7/32",
		},
		{
			name:    "no ip yields nil spec (mac still resolved)",
			cfg:     NetworkConfig{MAC: "aa:aa:aa:aa:aa:aa"},
			mMAC:    "02:00:00:00:80:01",
			wantMAC: "02:00:00:00:80:01", wantIPCIDR: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mac, spec := tc.cfg.Effective(tc.mMAC, tc.mIP)
			if mac != tc.wantMAC {
				t.Errorf("mac = %q, want %q", mac, tc.wantMAC)
			}
			if tc.wantIPCIDR == "" {
				if spec != nil {
					t.Errorf("spec = %+v, want nil", spec)
				}
				return
			}
			if spec == nil {
				t.Fatalf("spec = nil, want IPCIDR %q", tc.wantIPCIDR)
			}
			if spec.IPCIDR != tc.wantIPCIDR {
				t.Errorf("IPCIDR = %q, want %q", spec.IPCIDR, tc.wantIPCIDR)
			}
			if tc.wantMTU != 0 && spec.MTU != tc.wantMTU {
				t.Errorf("MTU = %d, want %d", spec.MTU, tc.wantMTU)
			}
			if tc.wantNexthop != "" && spec.Nexthop != tc.wantNexthop {
				t.Errorf("Nexthop = %q, want %q", spec.Nexthop, tc.wantNexthop)
			}
		})
	}
}
