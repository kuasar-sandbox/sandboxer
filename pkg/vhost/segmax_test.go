package vhost

import (
	"encoding/binary"
	"net"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func ringState(value uint32) *Message {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p[4:], value)
	return &Message{Payload: p}
}

func TestSegMaxRingSize(t *testing.T) {
	for _, size := range []uint32{1, 64, 65, 66, 128} {
		for _, featureFirst := range []bool{false, true} {
			for _, features := range []uint64{bitVhostProtocolFeatures, bitVhostProtocolFeatures | bitVirtioBlkSegMax} {
				s := NewServer("", &memBackend{}, nil)
				// ENABLE may arrive before either feature or size. Zero in
				// the unconfigured queue is not the peer's chosen ring size.
				if err := s.handleSetVringEnable(ringState(1)); err != nil {
					t.Fatal(err)
				}
				setFeatures := func() error { return s.handleSetFeatures(nil, &Message{Payload: minimalProfileU64(features)}) }
				setNum := func() error { return s.handleSetVringNum(ringState(size)) }
				first, second := setNum, setFeatures
				if featureFirst {
					first, second = setFeatures, setNum
				}
				if err := first(); err != nil {
					t.Fatal(err)
				}
				err := second()
				wantErr := features&bitVirtioBlkSegMax != 0 && size < 66
				if (err != nil) != wantErr {
					t.Fatalf("features=%#x size=%d featuresFirst=%t: %v", features, size, featureFirst, err)
				}
				if err == nil && (s.features != features || s.queues[0].num != size) {
					t.Fatal("negotiated state lost")
				}
			}
		}
	}
	for _, num := range []uint32{0, 65536, ^uint32(0)} {
		s := NewServer("", &memBackend{}, nil)
		if err := s.handleSetVringNum(ringState(num)); err == nil {
			t.Fatalf("accepted invalid explicit num=%d", num)
		}
	}
}

func TestSegMaxWorkerStartsAfterFinalHandshakeField(t *testing.T) {
	for _, last := range []string{"features", "num", "addr", "kick", "call", "enable", "memory"} {
		t.Run(last, func(t *testing.T) {
			s := NewServer("", &memBackend{}, nil)

			t.Cleanup(s.Stop)
			addr := make([]byte, 40)
			binary.LittleEndian.PutUint64(addr[8:], 16)
			binary.LittleEndian.PutUint64(addr[16:], 2176)
			binary.LittleEndian.PutUint64(addr[24:], 2048)
			fd := func() *Message {
				value, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
				if err != nil {
					t.Fatal(err)
				}
				return &Message{Payload: make([]byte, 8), Fds: []int{value}}
			}
			fields := map[string]func() error{
				"memory": func() error { return setRingTestMemory(t, s) },
				"features": func() error {
					return s.handleSetFeatures(nil, &Message{Payload: minimalProfileU64(bitVhostProtocolFeatures | bitVirtioBlkSegMax)})
				},
				"num":    func() error { return s.handleSetVringNum(ringState(128)) },
				"addr":   func() error { return s.handleSetVringAddr(&Message{Payload: addr}) },
				"kick":   func() error { return s.handleSetVringKick(fd()) },
				"call":   func() error { return s.handleSetVringCall(fd()) },
				"enable": func() error { return s.handleSetVringEnable(ringState(1)) },
			}
			for _, field := range []string{"features", "num", "addr", "kick", "call", "enable", "memory"} {
				if field != last {
					if err := fields[field](); err != nil {
						t.Fatal(err)
					}
				}
			}
			if s.queues[0].stop != nil {
				t.Fatal("early start")
			}
			if err := fields[last](); err != nil {
				t.Fatal(err)
			}
			if s.queues[0].stop == nil {
				t.Fatal("worker did not start")
			}
		})
	}
}

func setRingTestMemory(t *testing.T, s *Server) error {
	t.Helper()
	fd, err := unix.MemfdCreate("ring", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, 4096); err != nil {
		t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	s.SetMemfd(st.Ino, make([]byte, 4096))
	p := make([]byte, 40)
	binary.LittleEndian.PutUint32(p, 1)
	binary.LittleEndian.PutUint64(p[16:], 4096)
	dup, err := unix.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	return s.handleSetMemTable(nil, &Message{Payload: p, Fds: []int{dup}})
}

func TestImplicitStartDisarmedByGetBase(t *testing.T) {
	s := NewServer("", &memBackend{}, nil)
	t.Cleanup(s.Stop)
	apply := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	fd := func() *Message {
		val, err := unix.Eventfd(0, unix.EFD_CLOEXEC)
		apply(err)
		return &Message{Payload: make([]byte, 8), Fds: []int{val}}
	}
	features := &Message{Payload: minimalProfileU64(0)}
	addr := make([]byte, 40)
	binary.LittleEndian.PutUint64(addr[8:], 16)
	binary.LittleEndian.PutUint64(addr[16:], 2176)
	binary.LittleEndian.PutUint64(addr[24:], 2048)
	apply(s.handleSetFeatures(nil, features))
	apply(setRingTestMemory(t, s))
	apply(s.handleSetVringNum(ringState(8)))
	apply(s.handleSetVringAddr(&Message{Payload: addr}))
	apply(s.handleSetVringCall(fd()))
	apply(s.handleSetVringKick(fd()))
	q := s.queues[0]
	if q.stop == nil {
		t.Fatal("implicit queue did not start")
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	apply(err)
	defer unix.Close(pair[1])
	f := os.NewFile(uintptr(pair[0]), "get-base")
	conn, err := net.FileConn(f)
	f.Close()
	apply(err)
	defer conn.Close()
	apply(s.handleGetVringBase(conn.(*net.UnixConn), ringState(0)))
	apply(setRingTestMemory(t, s))
	apply(s.handleSetFeatures(nil, features))
	apply(s.handleSetVringNum(ringState(8)))
	apply(s.handleSetVringAddr(&Message{Payload: addr}))
	if q.stop != nil {
		t.Fatal("reconfiguration reactivated stopped queue")
	}
	apply(s.handleSetVringKick(fd()))
	if q.stop == nil {
		t.Fatal("fresh kick did not restart implicit queue")
	}
}
