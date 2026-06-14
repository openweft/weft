//go:build linux

package floatingipl2

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// sendGratuitousARP emits a gratuitous ARP (gARP) frame on parent
// announcing that mv carries ip. The point of gARP : after a VM
// migrates to this host, switches on the path have stale CAM
// entries pointing to the old host's MAC. A broadcast gARP frame
// with sender = target = the floating IP, source MAC = mv's MAC
// makes every switch on the broadcast domain refresh its CAM
// within milliseconds.
//
// Frame layout :
//
//	[14 B ethernet]  dst=ff:ff:ff:ff:ff:ff  src=<mv-mac>  type=0x0806
//	[28 B ARP]       htype=1 ptype=0x0800 hlen=6 plen=4 op=1 (request)
//	                 sha=<mv-mac> spa=<ip> tha=00:00:00:00:00:00 tpa=<ip>
//
// We send on the PARENT interface (not the macvlan) so the frame
// hits the trunk uplink with the right VLAN tag (the kernel adds
// the tag on the way out when the parent is a VLAN sub-interface).
// For untagged setups parent == bare NIC, also fine.
//
// IPv4 only — gARP doesn't exist for IPv6 (Neighbor Discovery
// has its own unsolicited NA mechanism that we don't implement
// yet ; v6 falls back to ARP-table aging on the upstream, which
// is slower but still works).
//
// Best-effort : socket open / send errors return an error to the
// caller but Apply swallows it. The address binding alone makes
// the path work after one ARP request → reply round-trip ; gARP
// just shortens the failover window.
func (p *LinuxProgrammer) sendGratuitousARP(parent, mv netlink.Link, ip netip.Addr) error {
	if !ip.Is4() {
		return nil // v6 gARP-equivalent not yet wired
	}
	mac := mv.Attrs().HardwareAddr
	if len(mac) != 6 {
		return fmt.Errorf("macvlan %s has no hardware address yet", mv.Attrs().Name)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("AF_PACKET socket: %w", err)
	}
	defer unix.Close(fd)

	addr := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  parent.Attrs().Index,
		Halen:    6,
	}
	copy(addr.Addr[:], broadcastMAC)

	frame := buildGratuitousARPFrame(mac, ip.As4())
	if err := unix.Sendto(fd, frame, 0, addr); err != nil {
		return fmt.Errorf("sendto %s: %w", parent.Attrs().Name, err)
	}
	return nil
}

// buildGratuitousARPFrame constructs the 42-byte ethernet+ARP
// frame. Pulled out of sendGratuitousARP so unit tests can pin
// the byte layout without touching AF_PACKET.
func buildGratuitousARPFrame(mac net.HardwareAddr, ip [4]byte) []byte {
	frame := make([]byte, 14+28)
	// Ethernet : dst=broadcast, src=mac, type=ARP.
	copy(frame[0:6], broadcastMAC)
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_ARP)
	// ARP.
	binary.BigEndian.PutUint16(frame[14:16], 1)               // htype = Ethernet
	binary.BigEndian.PutUint16(frame[16:18], 0x0800)          // ptype = IPv4
	frame[18] = 6                                              // hlen
	frame[19] = 4                                              // plen
	binary.BigEndian.PutUint16(frame[20:22], 1)               // op = request (gARP convention)
	copy(frame[22:28], mac)                                    // sha = sender MAC
	copy(frame[28:32], ip[:])                                  // spa = sender IP
	// tha = 00:00:00:00:00:00 (already zero from make)
	copy(frame[38:42], ip[:])                                  // tpa = same IP (gARP marker)
	return frame
}

var broadcastMAC = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// htons swaps a uint16 from host to network byte order. AF_PACKET
// protocol field expects network byte order ; unix.ETH_P_ARP is
// the host-order constant.
func htons(h uint16) uint16 {
	return (h<<8)&0xff00 | h>>8
}

// silence the unused-import warning if syscall isn't otherwise
// touched ; the file targets linux only.
var _ = syscall.AF_PACKET
