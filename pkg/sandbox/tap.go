package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DefaultNetQueuePairs is the queue-pair count of a CH tap-name-mode net
// device (CH NetConfig.num_queues counts virtqueues = 2×pairs; the restore
// net_fds rebind supplies one fd per pair).
const DefaultNetQueuePairs = 1

// VerifyTAP confirms that the named TAP/network interface exists. We do
// not create or modify the interface; the host platform is expected to
// provision TAPs ahead of sandbox launch.
func VerifyTAP(name string) error {
	if name == "" {
		return fmt.Errorf("tap: name is empty")
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return fmt.Errorf("tap: interface %s not found: %w", name, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("tap: interface %s is down", name)
	}
	return nil
}

// tapIfreq mirrors struct ifreq for the TUNSETIFF flags union member.
type tapIfreq struct {
	Name  [unix.IFNAMSIZ]byte
	Flags uint16
	_     [22]byte // ifru union padding to the full 40-byte ifreq size
}

// OpenTAPFDs attaches n queues of the named TAP and returns the open queue
// fds, configured to match what cloud-hypervisor's net_util produces for the
// device (vnet header size 12 = sizeof(virtio_net_hdr_v1)). The caller owns
// the fds: pass them to CH as extra files (fds 4.. in the run/restore
// layouts) and reference them via --net fd= / --restore net_fds=[_net0@[..]].
// n must match the queue count of the restored device — CH tap-name devices
// default to 2 (DEFAULT_NET_NUM_QUEUES).
//
// Used on the restore path in tap-name mode: CH --restore re-opens the tap
// name serialized in the snapshot VM state, and when that tap is already
// attached (concurrent restore of the same snapshot) the failure surfaces
// only deep inside CH as a bare "ConfigureTap: EBUSY" after the VM state
// has been loaded (sandboxer#161). Binding fresh fds to the restore
// config's tap and re-binding them onto the restored _net0 device via
// net_fds (CH patch 0008) makes concurrent tap-name-mode restores safe.
//
// Attach requires IFF_VNET_HDR (with an IFF_MULTI_QUEUE first attempt for
// MQ-provisioned taps): CH's inherited-fd net backend needs virtio-net
// header framing on the fd, and a device that rejects the vnet attach
// cannot back a restored net device — it is rejected explicitly. Only a
// genuinely held tap (attached by another process) or a vnet-incapable
// provisioning returns an error.
func OpenTAPFDs(name string, pairs int) ([]*os.File, error) {
	if err := VerifyTAP(name); err != nil {
		return nil, err
	}
	files := make([]*os.File, 0, pairs)
	attach := func(f *os.File, flags uint16) syscall.Errno {
		req := tapIfreq{Flags: flags}
		copy(req.Name[:], name)
		_, _, en := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.TUNSETIFF, uintptr(unsafe.Pointer(&req)))
		if en != 0 {
			return en
		}
		return 0
	}
	const virtioNetHdrV1Size = 12
	for i := 0; i < pairs; i++ {
		f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
		if err != nil {
			for _, of := range files {
				of.Close()
			}
			return nil, fmt.Errorf("tap: open /dev/net/tun: %w", err)
		}
		// Flag variants to try in order: the interface may or may not have
		// been provisioned multi-queue. Only IFF_VNET_HDR attaches are
		// accepted — CH's inherited-fd net backend requires virtio-net
		// header framing on the fd, and TUNSETVNETHDRSZ below only sizes
		// that framing, it does not enable it. A device that rejects
		// IFF_VNET_HDR attach (provisioned without vnet support) cannot
		// serve this restore and is rejected explicitly rather than
		// returning an fd with malformed framing. EINVAL = flag mismatch
		// with the device, EBUSY = queue held or no free queue.
		variants := []uint16{
			unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR | unix.IFF_MULTI_QUEUE,
			unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR,
		}
		var lastErr syscall.Errno
		attached := false
		for _, flags := range variants {
			en := attach(f, flags)
			if en == 0 {
				attached = true
				break
			}
			if en == syscall.EBUSY || en == syscall.EINVAL {
				lastErr = en
				continue
			}
			f.Close()
			closeAll(files)
			return nil, fmt.Errorf("tap: TUNSETIFF %s queue %d: %w", name, i, en)
		}
		if !attached {
			f.Close()
			closeAll(files)
			if errors.Is(lastErr, syscall.EBUSY) {
				return nil, fmt.Errorf("tap: interface %s is held by another process or has no free queue for restore (concurrent restore of one snapshot requires a distinct, multi_queue-capable tap per restore)", name)
			}
			return nil, fmt.Errorf("tap: interface %s does not accept IFF_VNET_HDR attach and cannot back a restored net device (re-provision with vnet support): %w", name, lastErr)
		}
		hdrsz := int32(virtioNetHdrV1Size)
		if _, _, en := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.TUNSETVNETHDRSZ, uintptr(unsafe.Pointer(&hdrsz))); en != 0 {
			f.Close()
			closeAll(files)
			return nil, fmt.Errorf("tap: TUNSETVNETHDRSZ %s queue %d: %w", name, i, en)
		}
		files = append(files, f)
	}
	return files, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}
