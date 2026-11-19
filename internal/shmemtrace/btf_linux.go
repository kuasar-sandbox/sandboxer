package shmemtrace

import (
	"fmt"
	"runtime"

	"github.com/cilium/ebpf/btf"
)

type layout struct {
	Arg                                   [3]int16
	Ret                                   int16
	MappingHost, InodeIno, InodeSB, SBDev int16
	FolioFlags, FolioPages                int16
	HeadBit                               uint32
}

func field(spec *btf.Spec, owner, name string, wantSize int) (int16, error) {
	var st *btf.Struct
	if err := spec.TypeByName(owner, &st); err != nil {
		return 0, err
	}
	found := map[uint32]bool{}
	var walk func(btf.Type, uint32, int) error
	walk = func(t btf.Type, base uint32, depth int) error {
		if depth > 12 {
			return fmt.Errorf("nested BTF member depth")
		}
		var members []btf.Member
		switch v := btf.UnderlyingType(t).(type) {
		case *btf.Struct:
			members = v.Members
		case *btf.Union:
			members = v.Members
		default:
			return nil
		}
		for _, m := range members {
			if m.Name == name {
				sz, err := btf.Sizeof(m.Type)
				if err != nil || (wantSize > 0 && sz != wantSize) || m.Offset%8 != 0 || m.BitfieldSize != 0 {
					return fmt.Errorf("unsupported BTF field %s.%s", owner, name)
				}
				found[base+uint32(m.Offset)/8] = true
			}
			if m.Name == "" && m.Offset%8 == 0 {
				if err := walk(m.Type, base+uint32(m.Offset)/8, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(st, 0, 0); err != nil {
		return 0, err
	}
	if len(found) != 1 {
		return 0, fmt.Errorf("BTF field %s.%s has %d candidates", owner, name, len(found))
	}
	for off := range found {
		if off > 32767 {
			return 0, fmt.Errorf("BTF offset too large")
		}
		return int16(off), nil
	}
	panic("unreachable")
}

func kernelLayout() (layout, error) {
	var l layout
	s, err := btf.LoadSpec("/sys/kernel/btf/vmlinux")
	if err != nil {
		return l, err
	}
	var f *btf.Func
	if err = s.TypeByName("shmem_add_to_page_cache", &f); err != nil {
		return l, err
	}
	proto, ok := btf.UnderlyingType(f.Type).(*btf.FuncProto)
	if !ok || len(proto.Params) < 3 {
		return l, fmt.Errorf("unexpected shmem prototype")
	}
	indexSize, e := btf.Sizeof(proto.Params[2].Type)
	if e != nil || indexSize != 8 {
		return l, fmt.Errorf("unsupported shmem index type")
	}
	resultType, ok := btf.UnderlyingType(proto.Return).(*btf.Int)
	if !ok || resultType.Size != 4 || resultType.Encoding&btf.Signed == 0 {
		return l, fmt.Errorf("unsupported shmem return type")
	}
	for i, want := range []string{"folio", "address_space"} {
		p, ok := btf.UnderlyingType(proto.Params[i].Type).(*btf.Pointer)
		if !ok {
			return l, fmt.Errorf("unexpected shmem parameter")
		}
		t, ok := btf.UnderlyingType(p.Target).(*btf.Struct)
		if !ok || t.Name != want {
			return l, fmt.Errorf("unexpected shmem parameter target")
		}
	}
	checks := []struct {
		owner, name string
		size        int
		dest        *int16
	}{{"address_space", "host", 8, &l.MappingHost}, {"inode", "i_ino", 8, &l.InodeIno}, {"inode", "i_sb", 8, &l.InodeSB}, {"super_block", "s_dev", 4, &l.SBDev}, {"folio", "flags", 8, &l.FolioFlags}, {"folio", "_folio_nr_pages", 4, &l.FolioPages}}
	for _, c := range checks {
		*c.dest, err = field(s, c.owner, c.name, c.size)
		if err != nil {
			return l, err
		}
	}
	switch runtime.GOARCH {
	case "amd64":
		for i, n := range []string{"di", "si", "dx"} {
			l.Arg[i], err = field(s, "pt_regs", n, 8)
			if err != nil {
				return l, err
			}
		}
		l.Ret, err = field(s, "pt_regs", "ax", 8)
		if err != nil {
			return l, err
		}
	case "arm64":
		base, e := field(s, "pt_regs", "regs", 31*8)
		if e != nil {
			return l, e
		}
		l.Arg = [3]int16{base, base + 8, base + 16}
		l.Ret = base
	default:
		return l, fmt.Errorf("unsupported architecture")
	}
	found := false
	for typ, err := range s.All() {
		if err != nil {
			return l, err
		}
		if e, ok := typ.(*btf.Enum); ok {
			for _, v := range e.Values {
				if v.Name == "PG_head" {
					if v.Value >= 64 {
						return l, fmt.Errorf("unsupported PG_head")
					}
					l.HeadBit = uint32(v.Value)
					found = true
				}
			}
		}
	}
	if !found {
		return l, fmt.Errorf("PG_head unavailable")
	}
	return l, nil
}
