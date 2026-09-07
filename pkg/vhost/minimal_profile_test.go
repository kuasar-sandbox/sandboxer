package vhost

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type minimalProfileBackend struct {
	*memBackend
	readOnly   bool
	configSize int64
	discards   int
	flushes    int
}

func (b *minimalProfileBackend) ReadOnly() bool { return b.readOnly }
func (b *minimalProfileBackend) Size() int64 {
	if b.configSize != 0 {
		return b.configSize
	}
	return b.memBackend.Size()
}
func (b *minimalProfileBackend) Discard(int64, int64) error {
	b.discards++
	return nil
}
func (b *minimalProfileBackend) Flush() error {
	b.flushes++
	return nil
}

func startMinimalProfileServer(t *testing.T, b Backend) (*Server, func() *net.UnixConn) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "v.sock")
	srv := NewServer(sock, b, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return srv, func() *net.UnixConn {
		t.Helper()
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			c.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
}

func minimalProfileRequest(t *testing.T, c *net.UnixConn, req uint32, payload []byte) *Message {
	t.Helper()
	if err := WriteMessage(c, Header{Request: req, Flags: FlagVersion1}, payload); err != nil {
		t.Fatal(err)
	}
	reply, err := ReadMessage(c)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Header.Request != req || !reply.IsReply() || len(reply.Fds) != 0 {
		closeFds(reply.Fds)
		t.Fatalf("unexpected reply: %+v", reply.Header)
	}
	return reply
}

func minimalProfileU64(v uint64) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint64(p, v)
	return p
}

func TestMinimalProfileFeatures(t *testing.T) {
	for _, ro := range []bool{false, true} {
		name := "rw"
		want := uint64(0x0000000140000200)
		if ro {
			name = "ro"
			want = 0x0000000140000220
		}
		t.Run(name, func(t *testing.T) {
			b := &minimalProfileBackend{memBackend: &memBackend{data: make([]byte, 4096)}, readOnly: ro}
			srv, dial := startMinimalProfileServer(t, b)
			if got := srv.advertisedFeatures(); got != want {
				t.Fatalf("advertised mask=%#x, want %#x", got, want)
			}
			c := dial()
			for _, tc := range []struct {
				req  uint32
				want uint64
			}{{MsgGetFeatures, want}, {MsgGetProtocolFeatures, 0x200}, {MsgGetQueueNum, 1}} {
				reply := minimalProfileRequest(t, c, tc.req, nil)
				if !bytes.Equal(reply.Payload, minimalProfileU64(tc.want)) {
					t.Fatalf("%s: got %x, want %#x", MsgName(tc.req), reply.Payload, tc.want)
				}
			}
		})
	}
}

func TestMinimalProfileFeatureSubsets(t *testing.T) {
	for _, ro := range []bool{false, true} {
		b := &minimalProfileBackend{memBackend: &memBackend{data: make([]byte, 512)}, readOnly: ro}
		srv := NewServer("unused", b, nil)
		for _, req := range []uint32{MsgSetFeatures, MsgSetProtocolFeatures} {
			supported := srv.advertisedFeatures()
			state := &srv.features
			if req == MsgSetProtocolFeatures {
				supported = bitProtocolConfig
				state = &srv.protocolFeatures
			}
			// Every subset, including zero, is accepted. Support is not an
			// obligation for the frontend to negotiate every advertised bit.
			for subset := supported; ; subset = (subset - 1) & supported {
				m := &Message{Header: Header{Request: req}, Payload: minimalProfileU64(subset)}
				if err := srv.handle(nil, m); err != nil || *state != subset {
					t.Fatalf("ro=%t %s subset=%#x: state=%#x err=%v", ro, MsgName(req), subset, *state, err)
				}
				if subset == 0 {
					break
				}
			}
			*state = supported
			for bit := uint(0); bit < 64; bit++ {
				unknown := uint64(1) << bit
				if unknown&supported != 0 {
					continue
				}
				m := &Message{Header: Header{Request: req}, Payload: minimalProfileU64(supported | unknown)}
				err := srv.handle(nil, m)
				if err == nil || *state != supported {
					t.Fatalf("ro=%t %s accepted unknown bit %#x or changed state: %#x, %v", ro, MsgName(req), unknown, *state, err)
				}
				for _, label := range []string{"requested=", "supported=", "unsupported="} {
					if !strings.Contains(err.Error(), label) {
						t.Fatalf("error missing %q: %v", label, err)
					}
				}
			}
			// Oversized payloads start with the valid but different zero
			// subset. Accepting just their first eight bytes would mutate
			// the prior state, so this assertion cannot pass accidentally.
			for _, size := range []int{0, 1, 7, 9, 16} {
				m := &Message{Header: Header{Request: req}, Payload: make([]byte, size)}
				if err := srv.handle(nil, m); err == nil || *state != supported {
					t.Fatalf("%s payload size %d accepted or changed state to %#x", MsgName(req), size, *state)
				}
			}
		}
	}
}

func TestMinimalProfileRejectReconnect(t *testing.T) {
	for _, req := range []uint32{MsgSetFeatures, MsgSetProtocolFeatures} {
		t.Run(MsgName(req), func(t *testing.T) {
			supported := uint64(0x140000200)
			if req == MsgSetProtocolFeatures {
				supported = 0x200
			}
			for _, tc := range []struct {
				name    string
				payload []byte
			}{
				{"unknown-bit", minimalProfileU64(1 << 63)},
				{"short", make([]byte, 7)},
				{"oversized", make([]byte, 9)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					srv, dial := startMinimalProfileServer(t, &memBackend{data: make([]byte, 512)})
					c := dial()
					if err := WriteMessage(c, Header{Request: req, Flags: FlagVersion1}, minimalProfileU64(supported)); err != nil {
						t.Fatal(err)
					}
					minimalProfileRequest(t, c, MsgGetFeatures, nil)
					if err := WriteMessage(c, Header{Request: req, Flags: FlagVersion1}, tc.payload); err != nil {
						t.Fatal(err)
					}
					// SET has no reply-ack. A following GET is a barrier:
					// invalid SET must close the connection, not allow setup
					// to continue. A timeout is not a successful rejection.
					_ = WriteMessage(c, Header{Request: MsgGetFeatures, Flags: FlagVersion1}, nil)
					if _, err := ReadMessage(c); err == nil {
						t.Fatal("invalid SET left connection usable")
					} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
						t.Fatalf("connection was not rejected before deadline: %v", err)
					}
					c.Close()
					c2 := dial()
					reply := minimalProfileRequest(t, c2, MsgGetFeatures, nil)
					if !bytes.Equal(reply.Payload, minimalProfileU64(0x140000200)) {
						t.Fatalf("reconnected profile=%x", reply.Payload)
					}
					srv.mu.Lock()
					features, protocol := srv.features, srv.protocolFeatures
					srv.mu.Unlock()
					if features != 0 || protocol != 0 {
						t.Fatalf("new connection inherited state: %#x / %#x", features, protocol)
					}
				})
			}
		})
	}
}

func minimalProfileConfigRequest(offset, size, flags uint32, bodyLen int) []byte {
	p := bytes.Repeat([]byte{0xa5}, 12+bodyLen)
	binary.LittleEndian.PutUint32(p[0:4], offset)
	binary.LittleEndian.PutUint32(p[4:8], size)
	binary.LittleEndian.PutUint32(p[8:12], flags)
	return p
}

func TestMinimalProfileConfig(t *testing.T) {
	const capacity = uint64(0x0102030405)
	b := &minimalProfileBackend{memBackend: &memBackend{}, configSize: int64(capacity * SectorSize)}
	_, dial := startMinimalProfileServer(t, b)
	c := dial()
	want := make([]byte, BlkConfigSize)
	binary.LittleEndian.PutUint64(want[:8], capacity)
	for _, flags := range []uint32{0, 1} {
		for _, tc := range []struct{ offset, size uint32 }{
			{0, BlkConfigSize}, {0, 8}, {1, 4}, {4, 4},
			{7, 2}, {8, BlkConfigSize - 8}, {BlkConfigSize - 1, 1},
		} {
			p := minimalProfileConfigRequest(tc.offset, tc.size, flags, int(tc.size))
			reply := minimalProfileRequest(t, c, MsgGetConfig, p)
			if len(reply.Payload) != len(p) || !bytes.Equal(reply.Payload[:12], p[:12]) ||
				!bytes.Equal(reply.Payload[12:], want[tc.offset:tc.offset+tc.size]) {
				t.Fatalf("offset=%d size=%d flags=%d: incorrect config %x", tc.offset, tc.size, flags, reply.Payload)
			}
		}
	}
	// Error replies have no payload and do not poison the next request.
	bad := [][]byte{
		nil, make([]byte, 4), make([]byte, 11),
		minimalProfileConfigRequest(0, 0, 0, 0),
		minimalProfileConfigRequest(0, 8, 0, 7),
		minimalProfileConfigRequest(0, 8, 0, 9),
		minimalProfileConfigRequest(0, BlkConfigSize+1, 0, BlkConfigSize+1),
		minimalProfileConfigRequest(BlkConfigSize, 1, 0, 1),
		minimalProfileConfigRequest(BlkConfigSize-1, 2, 0, 2),
		minimalProfileConfigRequest(^uint32(0), 8, 0, 8),
		minimalProfileConfigRequest(0, ^uint32(0), 0, 0),
		minimalProfileConfigRequest(0, MaxPayloadSize, 0, 0),
	}
	for i, p := range bad {
		reply := minimalProfileRequest(t, c, MsgGetConfig, p)
		if len(reply.Payload) != 0 || reply.Header.Size != 0 {
			t.Fatalf("malformed case %d returned successful config: %x", i, reply.Payload)
		}
		minimalProfileRequest(t, c, MsgGetFeatures, nil)
	}
}

func TestMinimalProfileBlockCommands(t *testing.T) {
	for _, kind := range []uint32{BlkTypeDiscard, BlkTypeWriteZero, BlkTypeIn, BlkTypeOut, BlkTypeFlush} {
		t.Run(BlkReqTypeName(kind), func(t *testing.T) {
			b := &minimalProfileBackend{memBackend: &memBackend{data: bytes.Repeat([]byte{0x5a}, 512)}}
			srv := NewServer("unused", b, nil)
			mem := make([]byte, 2048)
			const uva = uint64(0x1000)
			const chainHead = 3
			srv.memTable.SetRegions([]MemRegion{{GuestPhysAddr: 0, UserspaceAddr: uva, MemorySize: uint64(len(mem)), mmapBytes: mem}})
			q := &virtq{num: 8, descAddr: uva, usedAddr: uva + 128, kickFd: -1, callFd: -1}
			putDesc := func(idx int, addr uint64, length uint32, flags, next uint16) {
				d := mem[idx*16 : (idx+1)*16]
				binary.LittleEndian.PutUint64(d[0:8], addr)
				binary.LittleEndian.PutUint32(d[8:12], length)
				binary.LittleEndian.PutUint16(d[12:14], flags)
				binary.LittleEndian.PutUint16(d[14:16], next)
			}
			binary.LittleEndian.PutUint32(mem[256:260], kind)
			putDesc(chainHead, 256, 16, descFlagNext, chainHead+1)
			dataLen := uint32(16)
			dataFlags := descFlagNext
			if kind == BlkTypeIn || kind == BlkTypeOut {
				dataLen = 512
				copy(mem[512:1024], bytes.Repeat([]byte{0xa5}, 512))
			} else {
				// Valid range payload: sector=0, num_sectors=1, flags=0.
				binary.LittleEndian.PutUint32(mem[520:524], 1)
			}
			if kind == BlkTypeIn {
				dataFlags |= descFlagWrite
			}
			putDesc(chainHead+1, 512, dataLen, dataFlags, chainHead+2)
			putDesc(chainHead+2, 1536, 1, descFlagWrite, 0)
			if kind == BlkTypeFlush {
				putDesc(chainHead, 256, 16, descFlagNext, chainHead+2)
			}
			mem[1536] = 0xff
			before := append([]byte(nil), b.data...)
			payloadBefore := append([]byte(nil), mem[512:512+int(dataLen)]...)
			n, err := srv.processChain(q, chainHead)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantLen := BlkStatusOK, 1
			if kind == BlkTypeDiscard || kind == BlkTypeWriteZero {
				wantStatus = BlkStatusUnsupp
				if !bytes.Equal(b.data, before) || !bytes.Equal(mem[512:512+int(dataLen)], payloadBefore) {
					t.Fatal("unsupported command modified disk or payload")
				}
			}
			if kind == BlkTypeIn {
				wantLen = 513
				if !bytes.Equal(mem[512:1024], before) || !bytes.Equal(b.data, before) {
					t.Fatal("read regression")
				}
			}
			if kind == BlkTypeOut && !bytes.Equal(b.data, payloadBefore) {
				t.Fatal("write regression")
			}
			if mem[1536] != wantStatus || n != wantLen {
				t.Fatalf("status=%d len=%d, want %d/%d", mem[1536], n, wantStatus, wantLen)
			}
			if err := srv.publishUsed(q, chainHead, uint32(n)); err != nil {
				t.Fatal(err)
			}
			if binary.LittleEndian.Uint16(mem[130:132]) != 1 ||
				binary.LittleEndian.Uint32(mem[132:136]) != chainHead ||
				binary.LittleEndian.Uint32(mem[136:140]) != uint32(wantLen) {
				t.Fatal("incorrect used-ring completion idx/id/len")
			}
			wantFlushes := 0
			if kind == BlkTypeFlush {
				wantFlushes = 1
			}
			if b.discards != 0 || b.flushes != wantFlushes {
				t.Fatalf("backend calls: discard=%d flush=%d, want 0/%d", b.discards, b.flushes, wantFlushes)
			}
		})
	}
}
