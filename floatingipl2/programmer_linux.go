//go:build linux

package floatingipl2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
)

// osOpenFileWO opens for write only ; pulled out so tests can
// stub it without dragging file mocking up the call stack. The
// production impl is os.OpenFile(WRONLY|TRUNC).
var osOpenFileWO = func(path string) (procWriter, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
}

type procWriter interface {
	WriteString(string) (int, error)
	Close() error
}

// LinuxProgrammer is the netlink-backed implementation. Safe for
// concurrent Apply calls — the mutex serialises the multi-step
// state mutation (list-existing + diff + apply) so two callers
// can't race interleaved netlink ops on overlapping interfaces.
type LinuxProgrammer struct {
	mu sync.Mutex
}

// NewLinuxProgrammer returns a Programmer that drives netlink +
// raw sockets. No setup work happens here ; the first Apply
// installs / removes interfaces as needed.
func NewLinuxProgrammer() *LinuxProgrammer { return &LinuxProgrammer{} }

// Apply reconciles the kernel state to match mappings. Idempotent
// : repeated calls with the same input do nothing visible. Whole-
// state replace : interfaces owned by this package that no longer
// appear in mappings are torn down.
//
// Steps :
//  1. Group mappings by NetworkUUID ; one macvlan per network,
//     all FIPs on it as /32 secondary addresses.
//  2. For each group : ensure VLAN sub-interface exists (if
//     VLAN > 0), ensure macvlan exists on top, ensure macvlan is
//     up, sync addresses (add new, remove stale).
//  3. For each weft-owned macvlan NOT in the new set : tear it
//     down (and any addresses it carried).
//  4. Emit gARP for every still-active IP so the switch CAM is
//     refreshed.
//
// Errors abort early — a partial apply is recoverable on the next
// call. The mutex stays held for the full Apply so a concurrent
// Apply waits rather than racing.
func (p *LinuxProgrammer) Apply(mappings []L2Mapping) error {
	if err := ValidateMappings(mappings); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// Group by network UUID — each unique network → one macvlan.
	type group struct {
		networkUUID     string
		vlan            int
		parentInterface string
		ips             []netip.Addr
	}
	groups := make(map[string]*group)
	for _, m := range mappings {
		g, ok := groups[m.NetworkUUID]
		if !ok {
			g = &group{networkUUID: m.NetworkUUID, vlan: m.VLAN, parentInterface: m.ParentInterface}
			groups[m.NetworkUUID] = g
		}
		// Cross-mapping invariants : same network → same vlan +
		// parent. The control plane should enforce this, but
		// defend in depth.
		if g.vlan != m.VLAN {
			return fmt.Errorf("network %s has conflicting vlan values: %d vs %d",
				m.NetworkUUID, g.vlan, m.VLAN)
		}
		if g.parentInterface != m.ParentInterface {
			return fmt.Errorf("network %s has conflicting parent_interface values: %q vs %q",
				m.NetworkUUID, g.parentInterface, m.ParentInterface)
		}
		addr, _ := netip.ParseAddr(m.PublicIP)
		g.ips = append(g.ips, addr)
	}

	// Snapshot every weft-owned macvlan currently on the host
	// so we know which to keep, which to add IPs to, which to
	// delete.
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	owned := map[string]netlink.Link{}
	for _, l := range links {
		if l.Type() != "macvlan" {
			continue
		}
		if !strings.HasPrefix(l.Attrs().Name, MacvlanPrefix) {
			continue
		}
		owned[l.Attrs().Name] = l
	}

	keep := map[string]struct{}{}
	for _, g := range groups {
		name := macvlanNameFor(g.networkUUID)
		keep[name] = struct{}{}
		parent, err := p.ensureParent(g.parentInterface, g.vlan)
		if err != nil {
			return fmt.Errorf("ensure parent for %s: %w", g.networkUUID, err)
		}
		mv, err := p.ensureMacvlan(name, parent)
		if err != nil {
			return fmt.Errorf("ensure macvlan %s: %w", name, err)
		}
		// Enable unsolicited NA so IPv6 addresses bound below
		// trigger the kernel to broadcast a neighbor advertisement
		// — the gARP equivalent for switch CAM refresh. Failing
		// here only hurts NDP propagation speed ; the binding
		// still works, so swallow + continue.
		_ = enableIPv6UnsolicitedNA(name)
		if err := p.syncAddresses(mv, g.ips); err != nil {
			return fmt.Errorf("sync addresses on %s: %w", name, err)
		}
		// gARP for every announced IP — switch CAM refresh.
		for _, ip := range g.ips {
			if err := p.sendGratuitousARP(parent, mv, ip); err != nil {
				// Don't fail Apply on gARP errors — the
				// address is bound, the next packet will
				// trigger normal ARP. Log later via the
				// caller's hook ; this path is best-effort.
				_ = err
			}
		}
	}

	// Tear down macvlans no longer needed.
	for name, link := range owned {
		if _, stay := keep[name]; stay {
			continue
		}
		if err := netlink.LinkDel(link); err != nil {
			return fmt.Errorf("delete stale macvlan %s: %w", name, err)
		}
	}
	return nil
}

// ensureParent returns the netlink.Link the macvlan should
// attach to : either the bare parent NIC (when vlan == 0) or the
// VLAN sub-interface "<parent>.<vlan>". The sub-interface is
// created on first use ; we never delete it (it may be shared or
// operator-managed).
func (p *LinuxProgrammer) ensureParent(parentName string, vlan int) (netlink.Link, error) {
	parent, err := netlink.LinkByName(parentName)
	if err != nil {
		return nil, fmt.Errorf("parent %q: %w", parentName, err)
	}
	if vlan == 0 {
		// Make sure parent is up.
		if parent.Attrs().Flags&net.FlagUp == 0 {
			if err := netlink.LinkSetUp(parent); err != nil {
				return nil, fmt.Errorf("set parent up: %w", err)
			}
		}
		return parent, nil
	}
	subName := fmt.Sprintf("%s.%d", parentName, vlan)
	if existing, err := netlink.LinkByName(subName); err == nil {
		// Already there — verify it's a VLAN on the right parent
		// + tag. If somebody named a non-VLAN link the same way,
		// refuse rather than silently overwrite.
		if v, ok := existing.(*netlink.Vlan); ok {
			if v.ParentIndex != parent.Attrs().Index {
				return nil, fmt.Errorf("existing %s has wrong parent index %d (want %d)",
					subName, v.ParentIndex, parent.Attrs().Index)
			}
			if v.VlanId != vlan {
				return nil, fmt.Errorf("existing %s has wrong vlan id %d (want %d)",
					subName, v.VlanId, vlan)
			}
			if existing.Attrs().Flags&net.FlagUp == 0 {
				if err := netlink.LinkSetUp(existing); err != nil {
					return nil, fmt.Errorf("set %s up: %w", subName, err)
				}
			}
			return existing, nil
		}
		return nil, fmt.Errorf("existing link %s is not a VLAN (%s)", subName, existing.Type())
	}
	// Create new.
	v := &netlink.Vlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        subName,
			ParentIndex: parent.Attrs().Index,
		},
		VlanId:       vlan,
		VlanProtocol: netlink.VLAN_PROTOCOL_8021Q,
	}
	if err := netlink.LinkAdd(v); err != nil {
		return nil, fmt.Errorf("create vlan %s: %w", subName, err)
	}
	if err := netlink.LinkSetUp(v); err != nil {
		return nil, fmt.Errorf("set %s up: %w", subName, err)
	}
	// Re-fetch to get the kernel-assigned index.
	return netlink.LinkByName(subName)
}

// enableIPv6UnsolicitedNA sets the kernel sysctl
// /proc/sys/net/ipv6/conf/<ifname>/ndisc_notify to 1 so the
// kernel emits an unsolicited Neighbor Advertisement for every
// IPv6 address bound to the interface. This is the IPv6
// equivalent of gratuitous ARP for switch CAM refresh.
//
// Best-effort : sysctl write can fail under unprivileged
// network namespaces (test runs without CAP_NET_ADMIN) — the
// caller swallows the error since the address binding alone
// still works after the standard NDP exchange completes.
func enableIPv6UnsolicitedNA(ifname string) error {
	path := fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/ndisc_notify", ifname)
	return writeProc(path, "1")
}

// writeProc writes one short value to a /proc sysctl file. Open-
// write-close so we don't carry the fd. Errors are returned to
// the caller ; permission denied bubbles up so the test harness
// can skip cleanly.
func writeProc(path, value string) error {
	f, err := osOpenFileWO(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(value)
	return err
}

// ensureMacvlan creates (or finds) the macvlan child on parent
// and returns its netlink.Link. Mode = bridge so multiple
// macvlans can share the same parent without packet drops
// between them.
func (p *LinuxProgrammer) ensureMacvlan(name string, parent netlink.Link) (netlink.Link, error) {
	if existing, err := netlink.LinkByName(name); err == nil {
		if m, ok := existing.(*netlink.Macvlan); ok {
			if m.ParentIndex != parent.Attrs().Index {
				// Different parent — recreate.
				if err := netlink.LinkDel(existing); err != nil {
					return nil, fmt.Errorf("delete stale %s: %w", name, err)
				}
			} else {
				if existing.Attrs().Flags&net.FlagUp == 0 {
					if err := netlink.LinkSetUp(existing); err != nil {
						return nil, fmt.Errorf("set %s up: %w", name, err)
					}
				}
				return existing, nil
			}
		} else {
			return nil, fmt.Errorf("existing link %s is not a macvlan (%s)", name, existing.Type())
		}
	}
	mv := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        name,
			ParentIndex: parent.Attrs().Index,
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}
	if err := netlink.LinkAdd(mv); err != nil {
		return nil, fmt.Errorf("create macvlan %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(mv); err != nil {
		return nil, fmt.Errorf("set %s up: %w", name, err)
	}
	return netlink.LinkByName(name)
}

// syncAddresses reconciles the IP set bound to link with the
// desired ips. Adds missing, removes extra. /32 for IPv4 ; /128
// for IPv6.
func (p *LinuxProgrammer) syncAddresses(link netlink.Link, ips []netip.Addr) error {
	existing, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("list addrs: %w", err)
	}
	want := make(map[string]netip.Addr, len(ips))
	for _, ip := range ips {
		want[ip.String()] = ip
	}
	have := make(map[string]netlink.Addr, len(existing))
	for _, a := range existing {
		// Skip link-local v6 the kernel auto-assigns.
		if a.IP.IsLinkLocalUnicast() {
			continue
		}
		have[a.IP.String()] = a
	}
	for s, a := range have {
		if _, keep := want[s]; keep {
			continue
		}
		if err := netlink.AddrDel(link, &a); err != nil {
			return fmt.Errorf("del %s: %w", s, err)
		}
	}
	for s, ip := range want {
		if _, already := have[s]; already {
			continue
		}
		mask := 32
		if ip.Is6() {
			mask = 128
		}
		add := &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   net.ParseIP(s),
				Mask: net.CIDRMask(mask, mask),
			},
		}
		if err := netlink.AddrAdd(link, add); err != nil {
			return fmt.Errorf("add %s: %w", s, err)
		}
	}
	return nil
}

// macvlanNameFor returns the deterministic kernel interface name
// for a network's macvlan. The full UUID is too long for Linux's
// 15-char IFNAMSIZ limit, so we use the first 8 hex chars of a
// SHA-256 (collision-improbable across one cluster).
func macvlanNameFor(networkUUID string) string {
	h := sha256.Sum256([]byte(networkUUID))
	// Linux caps interface names at IFNAMSIZ-1 = 15 bytes; the kernel rejects
	// a longer name with ERANGE at LinkAdd. "wft-mvl-" is 8 bytes, so the hash
	// suffix must be ≤ 7 hex chars (28 bits, collision-improbable per host).
	return MacvlanPrefix + hex.EncodeToString(h[:4])[:maxMacvlanHashHex]
}

// maxMacvlanHashHex keeps MacvlanPrefix+hash within the 15-byte interface-name
// limit (len("wft-mvl-") == 8, 8+7 == 15).
const maxMacvlanHashHex = 7

// Compile-time check.
var _ Programmer = (*LinuxProgrammer)(nil)
