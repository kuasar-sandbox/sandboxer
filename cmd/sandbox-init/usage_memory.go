package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/kuasar-sandbox/sandboxer/pkg/proto"
)

func usageUint(v uint64) *uint64 { return &v }

func usageReadFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2*1024*1024 {
		return nil, errors.New("oversized allocator input")
	}
	return b, nil
}

func readUsageMemory() (proto.UsageMemory, error) {
	zone, err := usageReadFile("/proc/zoneinfo")
	if err != nil {
		return proto.UsageMemory{}, err
	}
	buddy, err := usageReadFile("/proc/buddyinfo")
	if err != nil {
		return proto.UsageMemory{}, err
	}
	return parseUsageMemory(zone, buddy, uint64(os.Getpagesize()))
}

type usageZone struct {
	name                                        string
	present, pcp, buddy                         uint64
	populated, presentSeen, buddySeen, pagesets bool
	cpus                                        map[string]bool
	cpu                                         string
}

func usageSum(a, b uint64) (uint64, error) {
	n, c := bits.Add64(a, b, 0)
	if c != 0 {
		return 0, errors.New("allocator overflow")
	}
	return n, nil
}

// parseUsageMemory aligns native counters by (node, zone). These sums only
// decode the raw allocator fields: GuestUsed and all usage arithmetic belong
// to sandbox-ctl. No spanned, MemAvailable or Balloon proc field is consumed.
func parseUsageMemory(zoneinfo, buddyinfo []byte, pageSize uint64) (proto.UsageMemory, error) {
	zones := make(map[string]*usageZone)
	var current *usageZone
	scan := bufio.NewScanner(bytes.NewReader(zoneinfo))
	scan.Buffer(make([]byte, 4096), 65536)
	for scan.Scan() {
		f := strings.Fields(scan.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) == 4 && f[0] == "Node" && f[2] == "zone" {
			node, err := strconv.ParseUint(strings.TrimSuffix(f[1], ","), 10, 32)
			if err != nil {
				return proto.UsageMemory{}, err
			}
			key := fmt.Sprintf("%d/%s", node, f[3])
			if zones[key] != nil || len(zones) >= 256 {
				return proto.UsageMemory{}, errors.New("duplicate/excess allocator zones")
			}
			current = &usageZone{name: f[3], cpus: make(map[string]bool)}
			zones[key] = current
			continue
		}
		if current == nil {
			return proto.UsageMemory{}, errors.New("allocator field before zone")
		}
		if f[0] == "pages" {
			current.populated = true
		}
		if len(f) == 2 && f[0] == "present" {
			if current.presentSeen {
				return proto.UsageMemory{}, errors.New("duplicate present")
			}
			n, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return proto.UsageMemory{}, err
			}
			current.present, current.presentSeen = n, true
		}
		if f[0] == "pagesets" {
			current.pagesets = true
			continue
		}
		if current.pagesets && len(f) == 2 && f[0] == "cpu:" {
			if current.cpu != "" && !current.cpus[current.cpu] {
				return proto.UsageMemory{}, errors.New("PCP count missing")
			}
			if _, err := strconv.ParseUint(f[1], 10, 32); err != nil {
				return proto.UsageMemory{}, err
			}
			if _, exists := current.cpus[f[1]]; exists {
				return proto.UsageMemory{}, errors.New("duplicate PCP CPU")
			}
			if len(current.cpus) >= 4096 {
				return proto.UsageMemory{}, errors.New("excess PCP CPUs")
			}
			current.cpu = f[1]
			current.cpus[f[1]] = false
		}
		if current.pagesets && len(f) == 2 && f[0] == "count:" {
			if current.cpu == "" || current.cpus[current.cpu] {
				return proto.UsageMemory{}, errors.New("unmatched PCP count")
			}
			n, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil {
				return proto.UsageMemory{}, err
			}
			current.pcp, err = usageSum(current.pcp, n)
			if err != nil {
				return proto.UsageMemory{}, err
			}
			current.cpus[current.cpu] = true
		}
	}
	if err := scan.Err(); err != nil {
		return proto.UsageMemory{}, err
	}
	scan = bufio.NewScanner(bytes.NewReader(buddyinfo))
	scan.Buffer(make([]byte, 4096), 65536)
	for scan.Scan() {
		f := strings.Fields(scan.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) < 5 || len(f) > 68 || f[0] != "Node" || f[2] != "zone" {
			return proto.UsageMemory{}, errors.New("invalid buddy row")
		}
		node, err := strconv.ParseUint(strings.TrimSuffix(f[1], ","), 10, 32)
		if err != nil {
			return proto.UsageMemory{}, err
		}
		z := zones[fmt.Sprintf("%d/%s", node, f[3])]
		if z == nil || z.buddySeen {
			return proto.UsageMemory{}, errors.New("buddy/zone domain mismatch")
		}
		z.buddySeen = true
		for order, raw := range f[4:] {
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || n > (^uint64(0)>>order) {
				return proto.UsageMemory{}, errors.New("buddy overflow")
			}
			z.buddy, err = usageSum(z.buddy, n<<order)
			if err != nil {
				return proto.UsageMemory{}, err
			}
		}
	}
	if err := scan.Err(); err != nil {
		return proto.UsageMemory{}, err
	}
	keys := make([]string, 0, len(zones))
	for key := range zones {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var present, buddy, pcp uint64
	domain := sha256.New()
	var cpuDomain string
	for _, key := range keys {
		z := zones[key]
		if z.name == "Device" || z.name == "PMEM" {
			continue
		}
		if !z.populated && !z.presentSeen && !z.buddySeen {
			continue
		} // Linux prints headers for unpopulated zones.
		if !z.presentSeen {
			return proto.UsageMemory{}, errors.New("present missing")
		}
		if z.present == 0 {
			if z.buddy != 0 || z.pcp != 0 {
				return proto.UsageMemory{}, errors.New("nonempty zero-present zone")
			}
			continue
		}
		switch z.name {
		case "DMA", "DMA32", "Normal":
		default:
			return proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageUnsupported}}, errors.New("unsupported RAM zone")
		}
		if !z.buddySeen || !z.pagesets || len(z.cpus) == 0 {
			return proto.UsageMemory{}, errors.New("allocator fields missing")
		}
		cpus := make([]string, 0, len(z.cpus))
		for cpu, seen := range z.cpus {
			if !seen {
				return proto.UsageMemory{}, errors.New("PCP count missing")
			}
			cpus = append(cpus, cpu)
		}
		sort.Strings(cpus)
		cpuset := strings.Join(cpus, ",")
		if cpuDomain != "" && cpuDomain != cpuset {
			return proto.UsageMemory{}, errors.New("PCP CPU domains differ")
		}
		cpuDomain = cpuset
		var err error
		present, err = usageSum(present, z.present)
		if err != nil {
			return proto.UsageMemory{}, err
		}
		buddy, err = usageSum(buddy, z.buddy)
		if err != nil {
			return proto.UsageMemory{}, err
		}
		pcp, err = usageSum(pcp, z.pcp)
		if err != nil {
			return proto.UsageMemory{}, err
		}
		fmt.Fprintf(domain, "%s:%d;", key, z.present)
	}
	if present == 0 {
		return proto.UsageMemory{}, errors.New("ordinary RAM unavailable")
	}
	return proto.UsageMemory{UsageReadState: proto.UsageReadState{Status: proto.UsageOK}, Domain: hex.EncodeToString(domain.Sum(nil)),
		PresentPages: usageUint(present), BuddyFreePages: usageUint(buddy), PCPFreePages: usageUint(pcp), PageSize: usageUint(pageSize)}, nil
}
