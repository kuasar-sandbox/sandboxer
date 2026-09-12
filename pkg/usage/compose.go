package usage

import (
	"errors"
	"time"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
	"github.com/kuasar-sandbox/sandboxer/pkg/resctl"
)

func memoryUsed(m proto.UsageMemory, balloon uint64) (uint64, error) {
	if m.Status != proto.UsageOK || m.Domain == "" || m.PresentPages == nil || m.BuddyFreePages == nil || m.PCPFreePages == nil || m.PageSize == nil {
		return 0, errors.New("memory fields unavailable")
	}
	p, b, c, size := *m.PresentPages, *m.BuddyFreePages, *m.PCPFreePages, *m.PageSize
	if size < 4096 || size > 65536 || size&(size-1) != 0 {
		return 0, errors.New("unsupported page unit")
	}
	if b > p || c > p-b {
		return 0, errors.New("negative non-free RAM")
	}
	used := p - b - c
	if used > ^uint64(0)/size {
		return 0, ErrOverflow
	}
	used *= size
	if balloon > used {
		return 0, errors.New("balloon exceeds non-free Guest RAM")
	}
	return used - balloon, nil
}

func filesystemUsed(f proto.UsageFilesystem) (uint64, error) {
	if f.Status != proto.UsageOK || f.Blocks == nil || f.BFree == nil || f.BlockSize == nil || f.FragmentSize == nil || f.Type == nil || *f.Type != 0xef53 {
		return 0, errors.New("filesystem fields unavailable")
	}
	unit := *f.FragmentSize
	if unit == 0 {
		unit = *f.BlockSize
	}
	if unit == 0 || *f.BFree > *f.Blocks {
		return 0, errors.New("invalid filesystem capacity/unit")
	}
	blocks := *f.Blocks - *f.BFree
	if blocks > ^uint64(0)/unit {
		return 0, ErrOverflow
	}
	return blocks * unit, nil
}

func jointWindow(start, end time.Time, a resctl.BalloonActualObservation, interval time.Duration) (time.Duration, bool) {
	if !a.Live || a.Sequence == 0 || a.Instance == 0 || a.Started.IsZero() || a.Finished.Before(a.Started) || end.Before(start) {
		return 0, false
	}
	if a.Started.Before(start) {
		start = a.Started
	}
	if a.Finished.After(end) {
		end = a.Finished
	}
	width := end.Sub(start)
	return width, width <= interval
}
