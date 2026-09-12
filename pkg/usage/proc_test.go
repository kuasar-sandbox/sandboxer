package usage

import (
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func procFixture(comm string) []byte {
	f := make([]string, 50)
	for i := range f {
		f[i] = "0"
	}
	f[0] = "S"
	f[11] = "111"
	f[12] = "7"
	f[17] = "3"
	f[19] = "222"
	f[40] = "99"
	return []byte("123 (" + comm + ") " + strings.Join(f, " ") + "\n")
}
func TestProcCommGuestAndRSS(t *testing.T) {
	for _, comm := range []string{"normal", "a b", "(with) nested ) (", "( ) )"} {
		s, err := parseProcStat(procFixture(comm))
		if err != nil || s.PID != 123 || s.Comm != comm || s.User != 111 || s.System != 7 || s.Guest != 99 || s.Start != 222 || s.Threads != 3 {
			t.Fatalf("%+v %v", s, err)
		}
	}
	for _, bad := range []string{"1 bad", "1 (bad) R 1", strings.Replace(string(procFixture("bad")), "111", "-1", 1)} {
		if _, err := parseProcStat([]byte(bad)); err == nil {
			t.Fatal(bad)
		}
	}
	a, f, err := parseRSS([]byte("Name: stuff\nRssAnon:\t123 kB\nRssFile: 456 kB\nRssShmem: 789 kB\n"))
	if err != nil || a != 123*1024 || f != 456*1024 {
		t.Fatalf("%d %d %v", a, f, err)
	}
	for _, bad := range []string{"RssAnon: 0 kB", "RssAnon: 1 MB\nRssFile: 2 kB", "RssAnon: 18446744073709551615 kB\nRssFile: 1 kB"} {
		if _, _, err := parseRSS([]byte(bad)); err == nil {
			t.Fatal(bad)
		}
	}
}

func TestAuxvNativeAndLayouts(t *testing.T) {
	for _, word := range []int{4, 8} {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			var b []byte
			for _, v := range []uint64{6, 4096, 17, 100, 0, 0} {
				part := make([]byte, word)
				if word == 4 {
					order.PutUint32(part, uint32(v))
				} else {
					order.PutUint64(part, v)
				}
				b = append(b, part...)
			}
			hz, err := parseAuxv(b, word, order)
			if err != nil || hz != 100 {
				t.Fatalf("%d %s %d %v", word, order, hz, err)
			}
			for n := 0; n < len(b); n++ {
				if _, err := parseAuxv(b[:n], word, order); err == nil {
					t.Fatalf("accepted truncation %d", n)
				}
			}
		}
	}
	for _, values := range [][]uint64{{17, 0, 0, 0}, {17, 100, 17, 100, 0, 0}, {6, 4096, 0, 0}, {0, 0, 17, 100}, {17, 100, 0, 1}} {
		var b []byte
		for _, v := range values {
			b = binary.LittleEndian.AppendUint64(b, v)
		}
		if _, err := parseAuxv(b, 8, binary.LittleEndian); err == nil {
			t.Fatal(values)
		}
	}
	if hz, err := clockTicks(); err != nil || hz == 0 {
		t.Fatalf("native AT_CLKTCK=%d %v", hz, err)
	}
}

func FuzzProcStat(f *testing.F) {
	f.Add(procFixture("a (b)"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 16*1024 {
			return
		}
		s, err := parseProcStat(b)
		if err == nil && s.PID < 1 {
			t.Fatal(fmt.Sprint(s))
		}
	})
}
