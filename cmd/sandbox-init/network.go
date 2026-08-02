// Guest IP-layer setup via raw netlink. Replaces the kernel's
// `ip=...` cmdline + CONFIG_IP_PNP path so we can drop those from
// the kernel.
//
// Three RTNETLINK messages are sufficient for static-IP setup:
//   1. RTM_NEWLINK with IFF_UP — bring iface up
//   2. RTM_NEWADDR with IFA_LOCAL/IFA_ADDRESS — assign the IP
//   3. RTM_NEWROUTE with RTA_GATEWAY — install default route (optional)
//
// Each carries NLM_F_REQUEST | NLM_F_ACK; we drain the ack and verify
// the kernel accepted (NLMSG_ERROR with code 0). Any non-zero error
// short-circuits and bubbles up.
//
// Implementation uses raw `golang.org/x/sys/unix` syscalls — already
// a dependency. ~250 lines, no new third-party libs.

package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"unsafe"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"golang.org/x/sys/unix"
)

// bringUpLoopback sets IFF_UP on the `lo` interface. The kernel creates the
// loopback device DOWN; bringing it up makes the kernel auto-assign the
// host-scope 127.0.0.1/8 and ::1/128, so no RTM_NEWADDR is needed. Done
// unconditionally on cold start (independent of any NetworkSpec) — guest apps
// that bind localhost would otherwise hit EADDRNOTAVAIL.
func bringUpLoopback() error {
	ifindex, err := readIfindex("lo")
	if err != nil {
		return fmt.Errorf("ifindex lo: %w", err)
	}
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}
	if err := nlSendLinkUp(fd, 1, ifindex, 0); err != nil {
		return fmt.Errorf("link up lo: %w", err)
	}
	return nil
}

// applyNetwork configures the guest IP layer on cold start: a fresh iface,
// additive (RTM_NEWADDR with NLM_F_EXCL, route with NLM_F_EXCL).
func applyNetwork(spec *proto.NetworkSpec) error { return applyNetworkMode(spec, false) }

// applyNetworkReplace re-applies the IP layer on restore. The iface is already
// configured from the snapshot, so it flushes the iface's existing global
// addresses first and uses REPLACE semantics for the new address + default
// route — letting a clone restored from a golden snapshot take a fresh
// network identity without inheriting the snapshot's IP.
func applyNetworkReplace(spec *proto.NetworkSpec) error { return applyNetworkMode(spec, true) }

// applyNetworkMode executes hostname + bring-iface-up(+MTU) + assign-IP +
// default-route in order. When replace is true it flushes existing global
// addresses before assigning and uses REPLACE instead of EXCL. Any step's
// failure aborts subsequent steps and returns the error.
func applyNetworkMode(spec *proto.NetworkSpec, replace bool) error {
	if spec == nil {
		return nil
	}
	if spec.Hostname != "" {
		if err := unix.Sethostname([]byte(spec.Hostname)); err != nil {
			return fmt.Errorf("sethostname %q: %w", spec.Hostname, err)
		}
	}
	if spec.IPCIDR == "" {
		return nil
	}
	iface := spec.Interface
	if iface == "" {
		iface = "eth0"
	}
	ifindex, err := readIfindex(iface)
	if err != nil {
		return fmt.Errorf("ifindex %s: %w", iface, err)
	}

	ip, ipnet, err := net.ParseCIDR(spec.IPCIDR)
	if err != nil {
		return fmt.Errorf("parse cidr %q: %w", spec.IPCIDR, err)
	}
	prefixLen, _ := ipnet.Mask.Size()
	family := unix.AF_INET
	if ip.To4() == nil {
		family = unix.AF_INET6
	}

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	if replace {
		return applyNetworkRestore(fd, iface, ifindex, family, ip, prefixLen, spec)
	}

	// cold start: sequential, additive
	seq := uint32(1)
	if err := nlSendLinkUp(fd, seq, ifindex, spec.MTU); err != nil {
		return fmt.Errorf("link up %s: %w", iface, err)
	}
	seq++
	if err := nlSendAddrAdd(fd, seq, ifindex, family, ip, prefixLen, false); err != nil {
		return fmt.Errorf("addr add %s on %s: %w", spec.IPCIDR, iface, err)
	}
	seq++
	if spec.Nexthop != "" {
		gw := net.ParseIP(spec.Nexthop)
		if gw == nil {
			return fmt.Errorf("parse nexthop %q: invalid IP", spec.Nexthop)
		}
		gwFamily := unix.AF_INET
		if gw.To4() == nil {
			gwFamily = unix.AF_INET6
		}
		if err := nlSendDefaultRoute(fd, seq, ifindex, gwFamily, gw, false); err != nil {
			return fmt.Errorf("default route via %s: %w", spec.Nexthop, err)
		}
	}
	return nil
}

// applyNetworkRestore is the optimized restore path. On restore the interface
// is already up (from snapshot) and may already carry the snapshot's old IP.
// Optimizations vs the naive sequential flush+replace:
//   - Skip RTM_NEWLINK when the link is already up AND the MTU matches.
//   - Dump existing addresses once; if the target IP/prefix is already
//     present, skip RTM_NEWADDR entirely (no change needed).
//   - Batch all remaining messages (DELADDR for stale addresses + NEWADDR +
//     NEWROUTE) into a single sendto + one ACK-drain loop, instead of one
//     send/recv round-trip per message.
func applyNetworkRestore(fd int, iface string, ifindex int32, family int, ip net.IP, prefix int, spec *proto.NetworkSpec) error {
	// 1. Skip link-up if already up (and MTU matches when specified).
	if !isLinkUp(iface) || (spec.MTU > 0 && readMTU(iface) != spec.MTU) {
		if err := nlSendLinkUp(fd, 1, ifindex, spec.MTU); err != nil {
			return fmt.Errorf("link up %s: %w", iface, err)
		}
	}

	// 2. Dump existing global-scope addresses to decide what to flush/skip.
	seq := uint32(2) // seq=1 used by linkup (if sent); dump starts at 2
	existing, err := nlDumpAddrs(fd, seq, ifindex, family)
	seq++
	if err != nil {
		return fmt.Errorf("dump addrs on %s: %w", iface, err)
	}
	// 3. Build one wire-level change batch: stale address deletions first,
	// followed by the target address and default route. Netlink can still
	// partially apply a batch, so every ACK is drained and any failure aborts
	// restore rather than allowing a clone to retain its old identity.
	var changeBatch []byte
	nChanges := 0
	targetExists := false
	for _, a := range existing {
		if a.ip.Equal(ip) && a.prefix == prefix {
			targetExists = true
			continue // keep — already the target
		}
		changeBatch = nlAppendMsg(changeBatch, unix.RTM_DELADDR, 0, seq, buildAddrBody(ifindex, family, a.prefix, a.ip))
		seq++
		nChanges++
	}

	// 4. Append NEWADDR + NEWROUTE to the same sendto batch.
	if !targetExists {
		changeBatch = nlAppendMsg(changeBatch, unix.RTM_NEWADDR, addFlags(true), seq, buildAddrBody(ifindex, family, prefix, ip))
		seq++
		nChanges++
	}
	if spec.Nexthop != "" {
		gw := net.ParseIP(spec.Nexthop)
		if gw == nil {
			return fmt.Errorf("parse nexthop %q: invalid IP", spec.Nexthop)
		}
		gwFamily := unix.AF_INET
		if gw.To4() == nil {
			gwFamily = unix.AF_INET6
		}
		changeBatch = nlAppendMsg(changeBatch, unix.RTM_NEWROUTE, addFlags(true), seq, buildRouteBody(ifindex, gwFamily, gw))
		seq++
		nChanges++
	}
	if err := nlSendBatch(fd, changeBatch, nChanges); err != nil {
		return fmt.Errorf("network change batch on %s: %w", iface, err)
	}
	return nil
}

// isLinkUp reads /sys/class/net/<iface>/flags and returns true iff IFF_UP set.
func isLinkUp(iface string) bool {
	data, err := os.ReadFile("/sys/class/net/" + iface + "/flags")
	if err != nil {
		return false
	}
	flags, err := strconv.ParseInt(strings.TrimSpace(string(data)), 0, 64)
	if err != nil {
		return false
	}
	return flags&unix.IFF_UP != 0
}

// readMTU reads /sys/class/net/<iface>/mtu.
func readMTU(iface string) int {
	data, err := os.ReadFile("/sys/class/net/" + iface + "/mtu")
	if err != nil {
		return 0
	}
	mtu, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return mtu
}

// readIfindex returns the kernel-assigned interface index for name via
// sysfs (one open+read+parse, no netlink round-trip).
func readIfindex(name string) (int32, error) {
	data, err := os.ReadFile("/sys/class/net/" + name + "/ifindex")
	if err != nil {
		return 0, err
	}
	idx, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return int32(idx), nil
}

// --- netlink message construction ---------------------------------

const nlAlignTo = 4

// nlAlign rounds len up to 4 bytes (NLMSG_ALIGN).
func nlAlign(n int) int { return (n + nlAlignTo - 1) &^ (nlAlignTo - 1) }

// nlAttr appends a TLV attribute to buf and returns the new buf.
func nlAttr(buf []byte, attrType uint16, value []byte) []byte {
	hdrSize := 4 // sizeof(rtattr)
	totalLen := hdrSize + len(value)
	padded := nlAlign(totalLen)
	a := make([]byte, padded)
	binary.LittleEndian.PutUint16(a[0:2], uint16(totalLen))
	binary.LittleEndian.PutUint16(a[2:4], attrType)
	copy(a[4:], value)
	return append(buf, a...)
}

// nlSend sends a single message with NLM_F_REQUEST|NLM_F_ACK and waits for
// the kernel ack (NLMSG_ERROR with err==0). It delegates to nlSendBatch with
// a one-message buffer, so the single- and batched-send paths share the same
// ACK-draining logic.
func nlSend(fd int, msgType uint16, flags uint16, seq uint32, body []byte) error {
	buf := nlAppendMsg(nil, msgType, flags, seq, body)
	return nlSendBatch(fd, buf, 1)
}

// nlAppendMsg builds one netlink message (NlMsghdr + body, NLM_F_REQUEST|
// NLM_F_ACK|flags) and appends it to buf. Returns the new buf. Used by both
// nlSend (single) and the batched restore path (multiple messages in one
// sendto).
func nlAppendMsg(buf []byte, msgType uint16, flags uint16, seq uint32, body []byte) []byte {
	const hdrSize = unix.SizeofNlMsghdr
	totalLen := hdrSize + len(body)
	old := len(buf)
	buf = append(buf, make([]byte, totalLen)...)
	hdr := (*unix.NlMsghdr)(unsafe.Pointer(&buf[old]))
	hdr.Len = uint32(totalLen)
	hdr.Type = msgType
	hdr.Flags = unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags
	hdr.Seq = seq
	hdr.Pid = 0
	copy(buf[old+hdrSize:], body)
	return buf
}

// nlSendBatch sends a buffer of one or more concatenated netlink messages in
// a single sendto and drains exactly expectAcks ACKs (NLMSG_ERROR). Each
// message in the buffer must carry NLM_F_ACK. The kernel processes messages
// in order, so ACKs arrive in the same order as requests.
//
// ALL expectAcks ACKs are drained even when one fails — this prevents a
// pending ACK from poisoning the socket for the next operation. The first
// non-zero error is returned, but the caller knows every message was
// processed by the kernel.
func nlSendBatch(fd int, buf []byte, expectAcks int) error {
	if len(buf) == 0 || expectAcks == 0 {
		return nil
	}
	if err := unix.Sendto(fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("sendto: %w", err)
	}
	rbuf := make([]byte, 8192)
	got := 0
	var firstErr error
	for got < expectAcks {
		n, _, err := unix.Recvfrom(fd, rbuf, 0)
		if err != nil {
			return fmt.Errorf("recvfrom: %w (drained %d/%d acks)", err, got, expectAcks)
		}
		if n < unix.SizeofNlMsghdr {
			return fmt.Errorf("short recv %d (drained %d/%d acks)", n, got, expectAcks)
		}
		// One recv may contain multiple ACKs (batched kernel reply).
		nACK, ackErr, parseErr := parseBatchACKs(rbuf[:n], expectAcks-got)
		if parseErr != nil {
			return fmt.Errorf("parse batch acks: %w (drained %d/%d acks)", parseErr, got, expectAcks)
		}
		got += nACK
		if firstErr == nil && ackErr != nil {
			firstErr = ackErr
		}
	}
	return firstErr
}

// parseBatchACKs parses up to limit NLMSG_ERROR acknowledgements from one
// recv buffer. It returns the first kernel error while still consuming every
// ACK in the buffer. Malformed messages are rejected instead of being treated
// as a partial success.
func parseBatchACKs(data []byte, limit int) (got int, firstErr, parseErr error) {
	const hdrSize = unix.SizeofNlMsghdr
	for len(data) >= hdrSize && got < limit {
		mh := (*unix.NlMsghdr)(unsafe.Pointer(&data[0]))
		l := int(mh.Len)
		if l < hdrSize || l > len(data) {
			return got, firstErr, fmt.Errorf("invalid netlink message length %d (buffer %d)", l, len(data))
		}
		if mh.Type == unix.NLMSG_ERROR {
			if l < hdrSize+4 {
				return got, firstErr, fmt.Errorf("short NLMSG_ERROR length %d", l)
			}
			errno := int32(binary.LittleEndian.Uint32(data[hdrSize : hdrSize+4]))
			if errno != 0 && firstErr == nil {
				firstErr = fmt.Errorf("netlink error %d (%s)", -errno, unix.Errno(-errno))
			}
			got++
		}
		aligned := nlAlign(l)
		if aligned > len(data) {
			return got, firstErr, fmt.Errorf("aligned netlink message length %d exceeds buffer %d", aligned, len(data))
		}
		data = data[aligned:]
	}
	if len(data) != 0 {
		return got, firstErr, fmt.Errorf("trailing %d-byte netlink data after %d acks", len(data), got)
	}
	return got, firstErr, nil
}

// nlSendLinkUp issues RTM_NEWLINK setting IFF_UP on ifindex, and — when
// mtu > 0 — an IFLA_MTU attribute to set the interface MTU.
func nlSendLinkUp(fd int, seq uint32, ifindex int32, mtu int) error {
	body := make([]byte, unix.SizeofIfInfomsg)
	ifi := (*unix.IfInfomsg)(unsafe.Pointer(&body[0]))
	ifi.Family = unix.AF_UNSPEC
	ifi.Index = ifindex
	ifi.Flags = unix.IFF_UP
	ifi.Change = unix.IFF_UP
	if mtu > 0 {
		raw := make([]byte, 4)
		binary.LittleEndian.PutUint32(raw, uint32(mtu))
		body = nlAttr(body, unix.IFLA_MTU, raw)
	}
	return nlSend(fd, unix.RTM_NEWLINK, 0, seq, body)
}

// addFlags returns the create flags for RTM_NEW* ops: REPLACE when replacing
// (restore), EXCL otherwise (cold; fail if it already exists).
func addFlags(replace bool) uint16 {
	if replace {
		return unix.NLM_F_CREATE | unix.NLM_F_REPLACE
	}
	return unix.NLM_F_CREATE | unix.NLM_F_EXCL
}

// buildAddrBody constructs the RTM_NEWADDR/RTM_DELADDR payload (IfAddrmsg +
// IFA_LOCAL + IFA_ADDRESS attributes).
func buildAddrBody(ifindex int32, family int, prefix int, ip net.IP) []byte {
	body := make([]byte, unix.SizeofIfAddrmsg)
	ifa := (*unix.IfAddrmsg)(unsafe.Pointer(&body[0]))
	ifa.Family = uint8(family)
	ifa.Prefixlen = uint8(prefix)
	ifa.Index = uint32(ifindex)
	ifa.Scope = unix.RT_SCOPE_UNIVERSE
	raw := addrBytes(family, ip)
	body = nlAttr(body, unix.IFA_LOCAL, raw)
	body = nlAttr(body, unix.IFA_ADDRESS, raw)
	return body
}

// nlSendAddrAdd issues RTM_NEWADDR with IFA_LOCAL + IFA_ADDRESS.
func nlSendAddrAdd(fd int, seq uint32, ifindex int32, family int, ip net.IP, prefix int, replace bool) error {
	return nlSend(fd, unix.RTM_NEWADDR, addFlags(replace), seq, buildAddrBody(ifindex, family, prefix, ip))
}

// buildRouteBody constructs the RTM_NEWROUTE payload for 0.0.0.0/0 via gw on
// ifindex (RtMsg + RTA_GATEWAY + RTA_OIF attributes).
func buildRouteBody(ifindex int32, family int, gw net.IP) []byte {
	body := make([]byte, unix.SizeofRtMsg)
	rt := (*unix.RtMsg)(unsafe.Pointer(&body[0]))
	rt.Family = uint8(family)
	rt.Dst_len = 0 // 0.0.0.0/0 or ::/0
	rt.Src_len = 0
	rt.Tos = 0
	rt.Table = unix.RT_TABLE_MAIN
	rt.Protocol = unix.RTPROT_BOOT
	rt.Scope = unix.RT_SCOPE_UNIVERSE
	rt.Type = unix.RTN_UNICAST
	rt.Flags = 0

	body = nlAttr(body, unix.RTA_GATEWAY, addrBytes(family, gw))

	// OIF (output interface) — 4 bytes LE int32 in NlAttr
	oifBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(oifBytes, uint32(ifindex))
	body = nlAttr(body, unix.RTA_OIF, oifBytes)
	return body
}

// nlSendDefaultRoute issues RTM_NEWROUTE for 0.0.0.0/0 (or ::/0) via gw on ifindex.
func nlSendDefaultRoute(fd int, seq uint32, ifindex int32, family int, gw net.IP, replace bool) error {
	return nlSend(fd, unix.RTM_NEWROUTE, addFlags(replace), seq, buildRouteBody(ifindex, family, gw))
}

// addrBytes returns the 4- or 16-byte wire form of ip for the family.
func addrBytes(family int, ip net.IP) []byte {
	if family == unix.AF_INET {
		return ip.To4()
	}
	return ip.To16()
}

type addrEntry struct {
	ip     net.IP
	prefix int
}

// nlDumpAddrs sends an RTM_GETADDR dump and collects the global-scope
// addresses of `family` on ifindex from the multipart reply.
func nlDumpAddrs(fd int, seq uint32, ifindex int32, family int) ([]addrEntry, error) {
	const hdrSize = unix.SizeofNlMsghdr
	body := make([]byte, unix.SizeofIfAddrmsg)
	ifa := (*unix.IfAddrmsg)(unsafe.Pointer(&body[0]))
	ifa.Family = uint8(family)

	buf := make([]byte, hdrSize+len(body))
	h := (*unix.NlMsghdr)(unsafe.Pointer(&buf[0]))
	h.Len = uint32(len(buf))
	h.Type = unix.RTM_GETADDR
	h.Flags = unix.NLM_F_REQUEST | unix.NLM_F_DUMP
	h.Seq = seq
	copy(buf[hdrSize:], body)
	if err := unix.Sendto(fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("sendto getaddr: %w", err)
	}

	var out []addrEntry
	rbuf := make([]byte, 8192)
	for {
		n, _, err := unix.Recvfrom(fd, rbuf, 0)
		if err != nil {
			return nil, fmt.Errorf("recvfrom getaddr: %w", err)
		}
		data := rbuf[:n]
		for len(data) >= hdrSize {
			mh := (*unix.NlMsghdr)(unsafe.Pointer(&data[0]))
			l := int(mh.Len)
			if l < hdrSize || l > len(data) {
				return nil, fmt.Errorf("getaddr dump: invalid netlink message length %d (buffer %d)", l, len(data))
			}
			switch mh.Type {
			case unix.NLMSG_DONE:
				return out, nil
			case unix.NLMSG_ERROR:
				errno := int32(binary.LittleEndian.Uint32(data[hdrSize : hdrSize+4]))
				if errno != 0 {
					return out, fmt.Errorf("getaddr dump: netlink error %d", -errno)
				}
				return out, nil
			case unix.RTM_NEWADDR:
				if e, ok := parseAddrEntry(data[hdrSize:l], ifindex, family); ok {
					out = append(out, e)
				}
			}
			aligned := nlAlign(l)
			if aligned > len(data) {
				return nil, fmt.Errorf("getaddr dump: aligned message length %d exceeds buffer %d", aligned, len(data))
			}
			data = data[aligned:]
		}
		if len(data) != 0 {
			return nil, fmt.Errorf("getaddr dump: trailing %d-byte netlink fragment", len(data))
		}
	}
}

// parseAddrEntry extracts (ip, prefix) from an RTM_NEWADDR payload iff it is a
// global-scope address of the given family on ifindex.
func parseAddrEntry(p []byte, ifindex int32, family int) (addrEntry, bool) {
	if len(p) < unix.SizeofIfAddrmsg {
		return addrEntry{}, false
	}
	ifa := (*unix.IfAddrmsg)(unsafe.Pointer(&p[0]))
	if int32(ifa.Index) != ifindex || int(ifa.Family) != family || ifa.Scope != unix.RT_SCOPE_UNIVERSE {
		return addrEntry{}, false
	}
	attrs := p[unix.SizeofIfAddrmsg:]
	var ip net.IP
	for len(attrs) >= 4 {
		alen := int(binary.LittleEndian.Uint16(attrs[0:2]))
		atype := binary.LittleEndian.Uint16(attrs[2:4])
		if alen < 4 || alen > len(attrs) {
			break
		}
		val := attrs[4:alen]
		if atype == unix.IFA_LOCAL || (atype == unix.IFA_ADDRESS && ip == nil) {
			ip = append(net.IP(nil), val...)
		}
		attrs = attrs[nlAlign(alen):]
	}
	if ip == nil {
		return addrEntry{}, false
	}
	return addrEntry{ip: ip, prefix: int(ifa.Prefixlen)}, true
}
