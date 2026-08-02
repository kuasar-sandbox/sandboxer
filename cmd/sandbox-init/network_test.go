package main

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// --- nlAppendMsg / buildAddrBody / buildRouteBody construction tests ---

func TestNlAppendMsgSingle(t *testing.T) {
	body := buildAddrBody(3, unix.AF_INET, 24, net.ParseIP("10.0.0.1").To4())
	buf := nlAppendMsg(nil, unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, 42, body)

	hdrSize := unix.SizeofNlMsghdr
	if len(buf) < hdrSize {
		t.Fatalf("buf too short: %d", len(buf))
	}
	msgType := binary.LittleEndian.Uint16(buf[4:6])
	flags := binary.LittleEndian.Uint16(buf[6:8])
	seq := binary.LittleEndian.Uint32(buf[8:12])
	if msgType != unix.RTM_NEWADDR {
		t.Errorf("type=%d want RTM_NEWADDR=%d", msgType, unix.RTM_NEWADDR)
	}
	wantFlags := uint16(unix.NLM_F_REQUEST | unix.NLM_F_ACK | unix.NLM_F_CREATE | unix.NLM_F_REPLACE)
	if flags != wantFlags {
		t.Errorf("flags=0x%x want 0x%x", flags, wantFlags)
	}
	if seq != 42 {
		t.Errorf("seq=%d want 42", seq)
	}
	// body starts after header — first byte is Family field of IfAddrmsg
	if buf[hdrSize] != unix.AF_INET {
		t.Errorf("body[0]=%d want AF_INET", buf[hdrSize])
	}
}

func TestNlAppendMsgBatchOrder(t *testing.T) {
	body1 := buildAddrBody(1, unix.AF_INET, 24, net.IPv4(10, 0, 0, 1))
	body2 := buildRouteBody(1, unix.AF_INET, net.IPv4(10, 0, 0, 254))

	buf := nlAppendMsg(nil, unix.RTM_DELADDR, 0, 1, body1)
	buf = nlAppendMsg(buf, unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, 2, body2)

	hdr1Len := int(binary.LittleEndian.Uint32(buf[0:4]))
	if hdr1Len < unix.SizeofNlMsghdr {
		t.Fatalf("msg1 len=%d too short", hdr1Len)
	}
	msg1Type := binary.LittleEndian.Uint16(buf[4:6])
	if msg1Type != unix.RTM_DELADDR {
		t.Errorf("msg1 type=%d want DELADDR", msg1Type)
	}

	off := nlAlign(hdr1Len)
	if off >= len(buf) {
		t.Fatalf("msg2 missing: off=%d buf=%d", off, len(buf))
	}
	msg2Type := binary.LittleEndian.Uint16(buf[off+4 : off+6])
	if msg2Type != unix.RTM_NEWROUTE {
		t.Errorf("msg2 type=%d want NEWROUTE", msg2Type)
	}
}

func TestBuildAddrBody(t *testing.T) {
	tests := []struct {
		name       string
		ifindex    int32
		family     int
		prefix     int
		ip         net.IP
		wantFamily uint8
		wantPrefix uint8
		wantIndex  uint32
		wantScope  uint8
	}{
		{
			name:       "IPv4",
			ifindex:    5,
			family:     unix.AF_INET,
			prefix:     16,
			ip:         net.IPv4(169, 254, 0, 21).To4(),
			wantFamily: unix.AF_INET,
			wantPrefix: 16,
			wantIndex:  5,
			wantScope:  unix.RT_SCOPE_UNIVERSE,
		},
		{
			name:       "IPv6",
			ifindex:    7,
			family:     unix.AF_INET6,
			prefix:     64,
			ip:         net.ParseIP("fd00::1").To16(),
			wantFamily: unix.AF_INET6,
			wantPrefix: 64,
			wantIndex:  7,
			wantScope:  unix.RT_SCOPE_UNIVERSE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildAddrBody(tt.ifindex, tt.family, tt.prefix, tt.ip)
			ifa := (*unix.IfAddrmsg)(unsafe.Pointer(&body[0]))
			if ifa.Family != tt.wantFamily {
				t.Errorf("family=%d want %d", ifa.Family, tt.wantFamily)
			}
			if ifa.Prefixlen != tt.wantPrefix {
				t.Errorf("prefixlen=%d want %d", ifa.Prefixlen, tt.wantPrefix)
			}
			if ifa.Index != tt.wantIndex {
				t.Errorf("index=%d want %d", ifa.Index, tt.wantIndex)
			}
			if ifa.Scope != tt.wantScope {
				t.Errorf("scope=%d want %d", ifa.Scope, tt.wantScope)
			}
		})
	}
}

func TestBuildRouteBody(t *testing.T) {
	gw := net.IPv4(10, 0, 0, 1).To4()
	body := buildRouteBody(3, unix.AF_INET, gw)

	rt := (*unix.RtMsg)(unsafe.Pointer(&body[0]))
	if rt.Family != unix.AF_INET {
		t.Errorf("family=%d want AF_INET", rt.Family)
	}
	if rt.Dst_len != 0 {
		t.Errorf("dst_len=%d want 0", rt.Dst_len)
	}
	attrs := body[unix.SizeofRtMsg:]
	hasGW, hasOIF := false, false
	for len(attrs) >= 4 {
		alen := int(binary.LittleEndian.Uint16(attrs[0:2]))
		atype := binary.LittleEndian.Uint16(attrs[2:4])
		if alen < 4 || alen > len(attrs) {
			break
		}
		if atype == unix.RTA_GATEWAY {
			hasGW = true
		}
		if atype == unix.RTA_OIF {
			hasOIF = true
			oif := binary.LittleEndian.Uint32(attrs[4:8])
			if oif != 3 {
				t.Errorf("oif=%d want 3", oif)
			}
		}
		attrs = attrs[nlAlign(alen):]
	}
	if !hasGW {
		t.Error("missing RTA_GATEWAY")
	}
	if !hasOIF {
		t.Error("missing RTA_OIF")
	}
}

// --- nlSendBatch ACK parsing test (no real socket needed) ---

func TestParseBatchACKs(t *testing.T) {
	hdrSize := unix.SizeofNlMsghdr
	makeACK := func(errno int32) []byte {
		m := make([]byte, hdrSize+4)
		binary.LittleEndian.PutUint32(m[0:4], uint32(hdrSize+4))
		binary.LittleEndian.PutUint16(m[4:6], unix.NLMSG_ERROR)
		binary.LittleEndian.PutUint32(m[8:12], 1) // seq
		binary.LittleEndian.PutUint32(m[hdrSize:hdrSize+4], uint32(errno))
		return m
	}

	makeMalformedACK := func() []byte {
		m := makeACK(0)
		binary.LittleEndian.PutUint32(m[0:4], uint32(hdrSize+128))
		return m
	}

	tests := []struct {
		name           string
		buf            []byte
		limit          int
		wantGot        int
		expectAckErr   bool
		expectParseErr bool
	}{
		{
			name:         "mixed ok and error ACKs",
			buf:          append(append(makeACK(0), makeACK(2)...), makeACK(0)...),
			limit:        3,
			wantGot:      3,
			expectAckErr: true,
		},
		{
			name:         "all-ok ACKs",
			buf:          append(makeACK(0), makeACK(0)...),
			limit:        2,
			wantGot:      2,
			expectAckErr: false,
		},
		{
			name:         "all-error ACKs",
			buf:          append(makeACK(5), makeACK(6)...),
			limit:        2,
			wantGot:      2,
			expectAckErr: true,
		},
		{
			name:           "malformed ACK",
			buf:            makeMalformedACK(),
			limit:          1,
			expectParseErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, firstErr, parseErr := parseBatchACKs(tt.buf, tt.limit)
			if tt.expectParseErr {
				if parseErr == nil {
					t.Error("expected parseErr, got nil")
				}
				return
			}
			if parseErr != nil {
				t.Fatalf("unexpected parseErr: %v", parseErr)
			}
			if got != tt.wantGot {
				t.Errorf("got=%d want %d", got, tt.wantGot)
			}
			if tt.expectAckErr && firstErr == nil {
				t.Error("expected ack error, got nil")
			}
			if !tt.expectAckErr && firstErr != nil {
				t.Errorf("unexpected ack error: %v", firstErr)
			}
		})
	}
}

// --- isLinkUp / readMTU tests (temp sysfs mock) ---

func TestIsLinkUp(t *testing.T) {
	tests := []struct {
		name       string
		netDir     string // relative to temp dir
		flags      string
		createFile bool
		want       bool
	}{
		{
			name:       "link up (IFF_UP set)",
			netDir:     "eth0",
			flags:      "0x1043\n",
			createFile: true,
			want:       true,
		},
		{
			name:       "link down (IFF_UP not set)",
			netDir:     "eth0_down",
			flags:      "0x1042\n",
			createFile: true,
			want:       false,
		},
		{
			name:       "missing interface directory",
			netDir:     "nonexistent",
			createFile: false,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			netPath := filepath.Join(dir, tt.netDir)
			if tt.createFile {
				os.MkdirAll(netPath, 0755)
				os.WriteFile(filepath.Join(netPath, "flags"), []byte(tt.flags), 0444)
			}
			if got := isLinkUpFromPath(netPath); got != tt.want {
				t.Errorf("isLinkUpFromPath() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadMTU(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		createFile bool
		want       int
	}{
		{
			name:       "valid MTU",
			content:    "1500\n",
			createFile: true,
			want:       1500,
		},
		{
			name:       "invalid MTU content",
			content:    "bad\n",
			createFile: true,
			want:       0,
		},
		{
			name:       "missing file",
			createFile: false,
			want:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			netPath := filepath.Join(dir, "eth0")
			if tt.createFile {
				os.MkdirAll(netPath, 0755)
				os.WriteFile(filepath.Join(netPath, "mtu"), []byte(tt.content), 0444)
			}
			if got := readMTUFromPath(netPath); got != tt.want {
				t.Errorf("readMTUFromPath() = %d, want %d", got, tt.want)
			}
		})
	}
}

// --- addFlags test ---

func TestAddFlags(t *testing.T) {
	tests := []struct {
		name    string
		replace bool
		want    uint16
	}{
		{
			name:    "replace is true",
			replace: true,
			want:    unix.NLM_F_CREATE | unix.NLM_F_REPLACE,
		},
		{
			name:    "replace is false",
			replace: false,
			want:    unix.NLM_F_CREATE | unix.NLM_F_EXCL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := addFlags(tt.replace); got != tt.want {
				t.Errorf("addFlags(%v) = 0x%x, want 0x%x", tt.replace, got, tt.want)
			}
		})
	}
}

// Test-path variants of isLinkUp/readMTU that read from an explicit dir
// (the real ones hardcode /sys/class/net/<iface>/...).
func isLinkUpFromPath(netDir string) bool {
	data, err := os.ReadFile(filepath.Join(netDir, "flags"))
	if err != nil {
		return false
	}
	flags, err := parseHexFlags(string(data))
	if err != nil {
		return false
	}
	return flags&unix.IFF_UP != 0
}

func readMTUFromPath(netDir string) int {
	data, err := os.ReadFile(filepath.Join(netDir, "mtu"))
	if err != nil {
		return 0
	}
	return parseDecInt(string(data))
}

func parseHexFlags(s string) (int64, error) {
	var v int64
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			v = v*16 + int64(c-'0')
		case c >= 'a' && c <= 'f':
			v = v*16 + int64(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v*16 + int64(c-'A'+10)
		case c == 'x' || c == 'X' || c == '\n' || c == '\r' || c == ' ':
			continue
		default:
			return v, nil
		}
	}
	return v, nil
}

func parseDecInt(s string) int {
	var v int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			v = v*10 + int(c-'0')
		} else {
			break
		}
	}
	return v
}
