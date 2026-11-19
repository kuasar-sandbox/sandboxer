package shmemtrace

import (
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
)

const eventSize = 344
const eventStack = int16(-368)
const kernelLimit = 64

// v0.20's Marshal snapshots Constant before projecting the atomic opcode into
// it. Set the FETCH bit explicitly as well, so the emitted instruction returns
// the pre-increment value instead of becoming an ordinary atomic add.
func fetchAdd(dst, src asm.Register) asm.Instruction {
	i := asm.FetchAdd.Mem(dst, src, asm.DWord, 0)
	i.Constant = 1
	return i
}

type event struct {
	ID, PIDTID, Index, Pages, Ktime, End uint64
	Result, StackSize                    int64
	Depth                                uint64
	Comm                                 [16]byte
	Stack                                [32]uint64
}

// Read kernel metadata only. The mapping's RAM bytes are never accessed.
func readKernel(dst asm.Register, ptr asm.Register, off int16, size int32, label string) asm.Instructions {
	out := asm.Instructions{asm.Mov.Reg(asm.R1, asm.RFP), asm.Add.Imm(asm.R1, -16), asm.Mov.Imm(asm.R2, size), asm.Mov.Reg(asm.R3, ptr), asm.Add.Imm(asm.R3, int32(off)), asm.FnProbeReadKernel.Call(), asm.JNE.Imm(asm.R0, 0, label)}
	s := asm.DWord
	if size == 4 {
		s = asm.Word
	}
	return append(out, asm.LoadMem(dst, asm.RFP, -16, s))
}

func statAdd(fd int, off int32, label string) asm.Instructions {
	return asm.Instructions{asm.StoreImm(asm.RFP, -16, 0, asm.Word), asm.LoadMapPtr(asm.R1, fd), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -16), asm.FnMapLookupElem.Call(), asm.JEq.Imm(asm.R0, 0, label), asm.Add.Imm(asm.R0, off), asm.Mov.Imm(asm.R1, 1), asm.StoreXAdd(asm.R0, asm.R1, asm.DWord), asm.Mov.Imm(asm.R0, 0).WithSymbol(label)}
}

func entryProgram(l layout, pending, stats *ebpf.Map, dev uint32, ino, start, end uint64) *ebpf.ProgramSpec {
	i := asm.Instructions{
		asm.Mov.Reg(asm.R6, asm.R1), asm.FnGetCurrentPidTgid.Call(), asm.StoreMem(asm.RFP, -8, asm.R0, asm.DWord),
		asm.LoadMapPtr(asm.R1, pending.FD()), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8), asm.FnMapLookupElem.Call(), asm.JEq.Imm(asm.R0, 0, "new"),
		// Preserve the outer call's return attribution if recursion ever occurs.
		asm.LoadMem(asm.R1, asm.R0, 64, asm.DWord), asm.Add.Imm(asm.R1, 1), asm.StoreMem(asm.R0, 64, asm.R1, asm.DWord),
	}
	i = append(i, statAdd(stats.FD(), 16, "nested_done")...)
	i = append(i, asm.Ja.Label("done"),
		asm.LoadMem(asm.R7, asm.R6, l.Arg[0], asm.DWord).WithSymbol("new"),
		asm.LoadMem(asm.R8, asm.R6, l.Arg[1], asm.DWord), asm.LoadMem(asm.R9, asm.R6, l.Arg[2], asm.DWord))
	i = append(i, readKernel(asm.R8, asm.R8, l.MappingHost, 8, "done")...)
	i = append(i, readKernel(asm.R1, asm.R8, l.InodeIno, 8, "done")...)
	i = append(i, asm.LoadImm(asm.R2, int64(ino), asm.DWord), asm.JNE.Reg(asm.R1, asm.R2, "done"))
	i = append(i, readKernel(asm.R8, asm.R8, l.InodeSB, 8, "done")...)
	i = append(i, readKernel(asm.R1, asm.R8, l.SBDev, 4, "done")...)
	i = append(i, asm.LoadImm(asm.R2, int64(dev), asm.DWord), asm.JNE.Reg(asm.R1, asm.R2, "done"))
	i = append(i, readKernel(asm.R1, asm.R7, l.FolioFlags, 8, "done")...)
	i = append(i, asm.LoadImm(asm.R2, int64(uint64(1)<<l.HeadBit), asm.DWord), asm.JSet.Reg(asm.R1, asm.R2, "large"), asm.Mov.Imm(asm.R8, 1), asm.Ja.Label("pages"))
	large := readKernel(asm.R8, asm.R7, l.FolioPages, 4, "done")
	large[0] = large[0].WithSymbol("large")
	i = append(i, large...)
	i = append(i, asm.JLT.Imm(asm.R8, 1, "done").WithSymbol("pages"), asm.JGT.Imm(asm.R8, 1048576, "done"),
		asm.LoadImm(asm.R1, int64(end/4096), asm.DWord), asm.JGE.Reg(asm.R9, asm.R1, "done"),
		asm.Mov.Reg(asm.R1, asm.R9), asm.Add.Reg(asm.R1, asm.R8), asm.JLT.Reg(asm.R1, asm.R9, "done"), asm.LoadImm(asm.R2, int64(start/4096), asm.DWord), asm.JLE.Reg(asm.R1, asm.R2, "done"),
		asm.StoreImm(asm.RFP, -16, 0, asm.Word), asm.LoadMapPtr(asm.R1, stats.FD()), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -16), asm.FnMapLookupElem.Call(), asm.JEq.Imm(asm.R0, 0, "done"),
		asm.Mov.Imm(asm.R1, 1), fetchAdd(asm.R0, asm.R1), asm.JGE.Imm(asm.R1, kernelLimit, "done"), asm.StoreMem(asm.RFP, -16, asm.R1, asm.DWord))
	for off := eventStack; off < eventStack+eventSize; off += 4 {
		i = append(i, asm.StoreImm(asm.RFP, off, 0, asm.Word))
	}
	i = append(i, asm.LoadMem(asm.R1, asm.RFP, -16, asm.DWord), asm.StoreMem(asm.RFP, eventStack, asm.R1, asm.DWord),
		asm.LoadMem(asm.R1, asm.RFP, -8, asm.DWord), asm.StoreMem(asm.RFP, eventStack+8, asm.R1, asm.DWord), asm.StoreMem(asm.RFP, eventStack+16, asm.R9, asm.DWord), asm.StoreMem(asm.RFP, eventStack+24, asm.R8, asm.DWord), asm.StoreImm(asm.RFP, eventStack+64, 1, asm.Word),
		asm.FnKtimeGetNs.Call(), asm.StoreMem(asm.RFP, eventStack+32, asm.R0, asm.DWord),
		asm.Mov.Reg(asm.R1, asm.RFP), asm.Add.Imm(asm.R1, int32(eventStack+72)), asm.Mov.Imm(asm.R2, 16), asm.FnGetCurrentComm.Call(),
		asm.Mov.Reg(asm.R1, asm.R6), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, int32(eventStack+88)), asm.Mov.Imm(asm.R3, 256), asm.Mov.Imm(asm.R4, 0), asm.FnGetStack.Call(), asm.StoreMem(asm.RFP, eventStack+56, asm.R0, asm.DWord),
		asm.LoadMapPtr(asm.R1, pending.FD()), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8), asm.Mov.Reg(asm.R3, asm.RFP), asm.Add.Imm(asm.R3, int32(eventStack)), asm.Mov.Imm(asm.R4, 1), asm.FnMapUpdateElem.Call(), asm.JEq.Imm(asm.R0, 0, "done"))
	i = append(i, statAdd(stats.FD(), 8, "pending_drop")...)
	i = append(i, asm.Mov.Imm(asm.R0, 0).WithSymbol("done"), asm.Return())
	return &ebpf.ProgramSpec{Name: "shmem_insert_in", Type: ebpf.Kprobe, License: "GPL", Instructions: i}
}

func returnProgram(l layout, pending, stats, events *ebpf.Map) *ebpf.ProgramSpec {
	i := asm.Instructions{asm.Mov.Reg(asm.R6, asm.R1), asm.LoadMem(asm.R7, asm.R6, l.Ret, asm.DWord), asm.LSh.Imm(asm.R7, 32), asm.ArSh.Imm(asm.R7, 32),
		asm.FnGetCurrentPidTgid.Call(), asm.StoreMem(asm.RFP, -8, asm.R0, asm.DWord), asm.LoadMapPtr(asm.R1, pending.FD()), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8), asm.FnMapLookupElem.Call(), asm.JEq.Imm(asm.R0, 0, "done"), asm.Mov.Reg(asm.R8, asm.R0),
		asm.LoadMem(asm.R1, asm.R8, 64, asm.DWord), asm.JLE.Imm(asm.R1, 1, "outer"), asm.Sub.Imm(asm.R1, 1), asm.StoreMem(asm.R8, 64, asm.R1, asm.DWord), asm.Ja.Label("done"),
		asm.StoreMem(asm.R8, 48, asm.R7, asm.DWord).WithSymbol("outer"), asm.FnKtimeGetNs.Call(), asm.StoreMem(asm.R8, 40, asm.R0, asm.DWord),
		asm.LoadMapPtr(asm.R1, events.FD()), asm.Mov.Reg(asm.R2, asm.R8), asm.Mov.Imm(asm.R3, eventSize), asm.Mov.Imm(asm.R4, 0), asm.FnRingbufOutput.Call(), asm.JEq.Imm(asm.R0, 0, "delete")}
	i = append(i, statAdd(stats.FD(), 24, "ring_drop")...)
	i = append(i, asm.LoadMapPtr(asm.R1, pending.FD()).WithSymbol("delete"), asm.Mov.Reg(asm.R2, asm.RFP), asm.Add.Imm(asm.R2, -8), asm.FnMapDeleteElem.Call(), asm.Mov.Imm(asm.R0, 0).WithSymbol("done"), asm.Return())
	return &ebpf.ProgramSpec{Name: "shmem_out", Type: ebpf.Kprobe, License: "GPL", Instructions: i}
}
