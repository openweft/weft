//go:build linux

package hostvip

// Linux Reconciler : attaches / detaches the VIP /32 (or /128) on the
// parent interface via netlink + broadcasts a gratuitous ARP from
// the local NIC's MAC. Mirrors the floatingipl2 pattern but skips
// the macvlan child : the VIP rides directly on the parent NIC so
// the kernel routes through it without any additional bridging.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// LinuxReconciler is the production hostvip.Reconciler used on Linux
// hosts. Stateless ; safe to share across multiple Controllers (one
// per VIP).
type LinuxReconciler struct{}

// NewLinuxReconciler returns a fresh LinuxReconciler. Always succeeds —
// it doesn't open any kernel handles until Bind / Unbind / AnnounceGARP
// are called.
func NewLinuxReconciler() *LinuxReconciler { return &LinuxReconciler{} }

// Bind adds the VIP /32 (or /128) to the named interface via netlink.
// Idempotent : an EEXIST from the kernel (the address is already
// present, e.g. agent restart while still leader) is swallowed.
func (LinuxReconciler) Bind(addr netip.Prefix, iface string) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("hostvip: link %s: %w", iface, err)
	}
	nlAddr, err := toNetlinkAddr(addr)
	if err != nil {
		return err
	}
	if err := netlink.AddrAdd(link, nlAddr); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil // already bound, treat as success
		}
		return fmt.Errorf("hostvip: addr add %s on %s: %w", addr, iface, err)
	}
	return nil
}

// Unbind removes the VIP from the interface. Missing / never-bound
// addresses don't error (ENODEV / ENOENT / EADDRNOTAVAIL → no-op).
func (LinuxReconciler) Unbind(addr netip.Prefix, iface string) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		// Interface gone (rare — operator removed the NIC) ; treat as no-op.
		if errors.Is(err, syscall.ENODEV) {
			return nil
		}
		return fmt.Errorf("hostvip: link %s: %w", iface, err)
	}
	nlAddr, err := toNetlinkAddr(addr)
	if err != nil {
		return err
	}
	if err := netlink.AddrDel(link, nlAddr); err != nil {
		if errors.Is(err, unix.EADDRNOTAVAIL) || errors.Is(err, syscall.ENOENT) {
			return nil // already unbound
		}
		return fmt.Errorf("hostvip: addr del %s on %s: %w", addr, iface, err)
	}
	return nil
}

// AnnounceGARP emits one gratuitous ARP frame so upstream switches +
// peer hosts on the L2 segment refresh their CAM / ARP cache to the
// current host's MAC. Without this, ARP cache TTLs delay the failover
// by 30-300 seconds depending on the OS.
//
// IPv4 only ; v6 ND announcement (unsolicited NA on ff02::1) is not
// yet implemented (same limitation as floatingipl2/garp_linux.go).
func (LinuxReconciler) AnnounceGARP(addr netip.Prefix, iface string) error {
	if !addr.Addr().Is4() {
		return nil // v6 path : ARP doesn't apply, ND not wired yet
	}
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("hostvip: link %s: %w", iface, err)
	}
	mac := link.Attrs().HardwareAddr
	if len(mac) != 6 {
		return fmt.Errorf("hostvip: iface %s has no MAC", iface)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htonsLocal(unix.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("hostvip: AF_PACKET socket: %w", err)
	}
	defer unix.Close(fd)

	sa := &unix.SockaddrLinklayer{
		Protocol: htonsLocal(unix.ETH_P_ARP),
		Ifindex:  link.Attrs().Index,
		Halen:    6,
	}
	copy(sa.Addr[:], broadcastMAC)

	frame := buildGratuitousARPFrame(mac, addr.Addr().As4())
	if err := unix.Sendto(fd, frame, 0, sa); err != nil {
		return fmt.Errorf("hostvip: sendto %s: %w", iface, err)
	}
	return nil
}

// toNetlinkAddr converts a netip.Prefix to the vishvananda/netlink
// IPNet shape that AddrAdd / AddrDel consume.
func toNetlinkAddr(p netip.Prefix) (*netlink.Addr, error) {
	if !p.IsValid() {
		return nil, fmt.Errorf("hostvip: invalid prefix %q", p)
	}
	bits := p.Bits()
	if p.Addr().Is4() {
		ip := p.Addr().As4()
		return &netlink.Addr{IPNet: &net.IPNet{
			IP:   net.IP(ip[:]),
			Mask: net.CIDRMask(bits, 32),
		}}, nil
	}
	ip := p.Addr().As16()
	return &netlink.Addr{IPNet: &net.IPNet{
		IP:   net.IP(ip[:]),
		Mask: net.CIDRMask(bits, 128),
	}}, nil
}

// buildGratuitousARPFrame builds the 42-byte Ethernet+ARP frame used
// by AnnounceGARP. The layout mirrors floatingipl2's identical
// helper ; kept local so hostvip doesn't depend on that package.
//
//	[14 B ethernet]  dst=ff:ff:ff:ff:ff:ff  src=<mac>  type=0x0806
//	[28 B ARP]       htype=1 ptype=0x0800 hlen=6 plen=4 op=1 (request)
//	                 sha=<mac> spa=<ip> tha=00:00:00:00:00:00 tpa=<ip>
func buildGratuitousARPFrame(mac net.HardwareAddr, ip [4]byte) []byte {
	frame := make([]byte, 14+28)
	copy(frame[0:6], broadcastMAC)
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_ARP)
	binary.BigEndian.PutUint16(frame[14:16], 1)      // htype = Ethernet
	binary.BigEndian.PutUint16(frame[16:18], 0x0800) // ptype = IPv4
	frame[18] = 6                                    // hlen
	frame[19] = 4                                    // plen
	binary.BigEndian.PutUint16(frame[20:22], 1)      // op = request (gARP)
	copy(frame[22:28], mac)                          // sha = sender MAC
	copy(frame[28:32], ip[:])                        // spa = sender IP
	// tha = 00:00:00:00:00:00 (already zero)
	copy(frame[38:42], ip[:]) // tpa = same IP (gARP marker)
	return frame
}

var broadcastMAC = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// htonsLocal swaps a uint16 from host to network byte order. AF_PACKET
// protocol field expects network byte order ; unix.ETH_P_ARP is the
// host-order constant. Named with the *Local suffix so it doesn't
// collide with floatingipl2.htons if the two ever share a vendor path.
func htonsLocal(h uint16) uint16 {
	return (h<<8)&0xff00 | h>>8
}
