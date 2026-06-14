// Package floatingipl2 is the L2 / VLAN attachment path for
// floating IPs in deployments where the establishment provides a
// VLAN trunk + subnet but no routing protocol (academic /
// enterprise networks where BGP peering isn't an option).
//
// Architecture :
//
//	weft (control plane)      ─ floating_ip.{mapped,unmapped,...}
//	                                       ▼
//	floatingipnat.Watcher    ─ ComputeLocalMappings (NAT side)
//	                       └── ComputeL2Mappings  (L2 side, NEW)
//	                                       ▼
//	LinuxL2Programmer.Apply   ─ netlink + AF_PACKET
//	                                       ▼
//	kernel : VLAN subinterface "<parent>.<vlan>"  (if vlan>0)
//	         macvlan child  "wft-mvl-<network-hash>"
//	         /32 address per FIP, secondary addresses
//	         gARP emitted on the parent NIC, broadcast destination
//
// One macvlan per network UUID (deterministic name), multiple /32s
// bound as secondary addresses. gARP fires for every IP on the
// last reconcile, so a switch CAM update propagates within ms.
// Reconciler is whole-state — Apply replaces the kernel set with
// the supplied mappings, removing what's no longer needed.
//
// VLAN subinterface is NOT torn down on empty mappings : it may
// be operator-managed or shared with other tenants. macvlans
// owned by this package use the "wft-mvl-" prefix so the
// reconciler can identify which to manage.
//
// This package complements floatingipnat (host-side NAT). For a
// VLAN-mode network the two reconcilers run in parallel : NAT
// rewrites the packet destination ; the macvlan + gARP makes the
// switch send the packet here in the first place.
package floatingipl2

import (
	"fmt"
	"net/netip"
)

// L2Mapping is one FIP-to-host attachment the programmer must
// install. NetworkUUID groups mappings for the same macvlan
// interface ; mappings with the same NetworkUUID share one
// kernel interface and add up as secondary /32 addresses.
type L2Mapping struct {
	// PublicIP is the floating-IP address bound to the macvlan
	// as a /32 (IPv4) or /128 (IPv6). Used both for the
	// address binding and as the gARP sender + target field.
	PublicIP string
	// NetworkUUID is the network the FIP belongs to. Used to
	// derive the macvlan interface name deterministically so
	// repeated Apply calls don't churn the interface set.
	NetworkUUID string
	// VLAN is the 802.1Q tag (1-4094) when the parent NIC
	// carries a trunk. 0 means untagged — the macvlan attaches
	// directly to ParentInterface without a sub-interface.
	VLAN int
	// ParentInterface is the host NIC name (e.g. "eth0",
	// "bond0"). Must exist on the host before Apply runs ; the
	// programmer does not create it.
	ParentInterface string
	// VMName is informational, included in log lines + as the
	// kernel interface alias on the macvlan so an operator can
	// trace a kernel object back to a tenant intent.
	VMName string
}

// Validate checks a single mapping has parseable PublicIP and
// the cross-field invariants. Called by ValidateMappings before
// Apply, so a malformed mapping never reaches netlink.
func (m L2Mapping) Validate() error {
	addr, err := netip.ParseAddr(m.PublicIP)
	if err != nil {
		return fmt.Errorf("public_ip %q: %w", m.PublicIP, err)
	}
	_ = addr
	if m.NetworkUUID == "" {
		return fmt.Errorf("empty network_uuid")
	}
	if m.ParentInterface == "" {
		return fmt.Errorf("empty parent_interface")
	}
	if m.VLAN < 0 || m.VLAN > 4094 {
		return fmt.Errorf("vlan out of range [0,4094]: %d", m.VLAN)
	}
	return nil
}

// Programmer is the platform abstraction over the netlink-or-stub
// backend. Apply is whole-state : the kernel macvlan set owned by
// this package is replaced atomically (per-NIC, not per-host) with
// the supplied mappings. Empty input removes every weft-owned
// macvlan from the host.
//
// Implementations :
//   - LinuxProgrammer (programmer_linux.go) — netlink + AF_PACKET.
//   - StubProgrammer  (programmer_other.go) — records the last
//     Apply payload for tests + cross-platform builds.
type Programmer interface {
	Apply(mappings []L2Mapping) error
}

// ValidateMappings runs per-mapping Validate + cross-mapping
// invariants : the same PublicIP can't appear twice (the kernel
// would refuse to bind a duplicate address). Returns the first
// error ; pulled out of Apply so the caller can surface clear
// errors before any netlink mutation.
func ValidateMappings(mappings []L2Mapping) error {
	seen := make(map[string]struct{})
	for i, m := range mappings {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("mapping[%d]: %w", i, err)
		}
		if _, dup := seen[m.PublicIP]; dup {
			return fmt.Errorf("mapping[%d]: public_ip %s appears twice", i, m.PublicIP)
		}
		seen[m.PublicIP] = struct{}{}
	}
	return nil
}

// MacvlanPrefix is the kernel interface name prefix the
// programmer reserves. Names look like "wft-mvl-<8-hex-hash>".
// Operators can `ip -d link show type macvlan` to spot them ;
// nothing else in openweft creates interfaces with this prefix.
const MacvlanPrefix = "wft-mvl-"
