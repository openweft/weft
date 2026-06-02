// Package floatingipnat is the host-side reconciler that converges
// a single nftables NAT table to the desired set of Floating-IP →
// private-IP mappings for every VM running on this host.
//
// Why host-side and not in the microVM (mirror of the firewall
// pipeline) ? Because a floating IP is a public-routable address
// the host receives traffic on ; DNAT must happen BEFORE the
// packet is forwarded to the VM's virtio/tap, otherwise the
// guest never sees it. SNAT for the reverse direction lives at
// the same hook so reply traffic egresses with the public source.
//
// One table per host : "ip weft-fip-nat" with two chains —
//   chain prerouting  { type nat hook prerouting  priority dstnat ;
//     <per-mapping DNAT rule>
//   }
//   chain postrouting { type nat hook postrouting priority srcnat ;
//     <per-mapping SNAT rule>
//   }
//
// Replace-set per netlink batch — same shape pkg/network in
// weft-microvm-init uses for the in-VM firewall. A missed reconcile
// self-heals on the next Apply ; an idle host (no mappings)
// converges to an empty table that's still present (so an external
// `nft list ruleset` lookup tells the operator the daemon is
// in charge of the namespace).
package floatingipnat

import (
	"fmt"
	"net/netip"
)

// NATMapping is one floating-IP-to-private-IP binding the host
// must NAT. PublicIP is the address the platform owns (a
// FloatingIP allocated from an edge network) ; PrivateIP is the
// address of the VM's port on its tenant network. Both must be
// the same family (IPv4 or IPv6).
type NATMapping struct {
	// PublicIP is the floating IP visible to the outside world.
	// DNAT replaces it with PrivateIP on inbound traffic ; SNAT
	// replaces PrivateIP with it on outbound.
	PublicIP string
	// PrivateIP is the VM port's address on its tenant network.
	// Reachable from the host via the tenant network's bridge /
	// veth / WireGuard interface — that part of the path is the
	// network driver's responsibility, not the reconciler's.
	PrivateIP string
	// VMName is informational, included in the rule comment so
	// `nft list ruleset` is operator-readable. Empty is fine.
	VMName string
}

// Validate checks the mapping has parseable IPs of matching
// family. Returns nil for a well-formed mapping.
func (m NATMapping) Validate() error {
	pub, err := netip.ParseAddr(m.PublicIP)
	if err != nil {
		return fmt.Errorf("public_ip %q: %w", m.PublicIP, err)
	}
	prv, err := netip.ParseAddr(m.PrivateIP)
	if err != nil {
		return fmt.Errorf("private_ip %q: %w", m.PrivateIP, err)
	}
	if pub.Is4() != prv.Is4() {
		return fmt.Errorf("public %s and private %s must be same family", m.PublicIP, m.PrivateIP)
	}
	return nil
}

// Reconciler is the platform abstraction over the nftables-or-stub
// data-plane backend. Apply is whole-state : the table is
// replaced atomically with rules derived from mappings. Passing
// an empty slice leaves an empty table behind (the table itself
// stays installed, so an operator-side `nft list table ip weft-
// fip-nat` confirms the daemon owns the namespace).
//
// Implementations :
//   * LinuxReconciler (reconciler_linux.go) — real netlink path
//     via github.com/google/nftables.
//   * StubReconciler  (reconciler_other.go) — no-op on darwin /
//     test, records the last Apply payload for assertions.
type Reconciler interface {
	Apply(mappings []NATMapping) error
}

// ValidateMappings checks every entry + rejects duplicates on
// PublicIP (a single public address can only forward to one
// private destination). Returns the first error encountered.
// Pulled out of Apply so the host-side caller can validate the
// input before reaching netlink — clearer errors, no half-applied
// state.
func ValidateMappings(mappings []NATMapping) error {
	seen := make(map[string]string)
	for i, m := range mappings {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("mapping[%d]: %w", i, err)
		}
		if prev, dup := seen[m.PublicIP]; dup {
			return fmt.Errorf("mapping[%d]: public_ip %s already mapped to %s", i, m.PublicIP, prev)
		}
		seen[m.PublicIP] = m.PrivateIP
	}
	return nil
}

// FilterToTargetSet is the host-side helper that turns the full
// platform floating-IP list into the subset whose target VMs are
// running on this host. The map argument is keyed by VM name
// and maps to the VM's private IP on its tenant network. VMs the
// host doesn't run drop out of the result.
//
// Pure (no IO, no nftables call) so the integration code can
// build the input as it sees fit (etcd watch, per-event delta,
// periodic full-sync) and rely on this for the actual
// publicIP → privateIP join.
func FilterToTargetSet(allMappings []ControlPlaneMapping, localVMs map[string]string) []NATMapping {
	out := make([]NATMapping, 0, len(allMappings))
	for _, m := range allMappings {
		privateIP, local := localVMs[m.VMName]
		if !local || privateIP == "" {
			continue
		}
		out = append(out, NATMapping{
			PublicIP:  m.PublicIP,
			PrivateIP: privateIP,
			VMName:    m.VMName,
		})
	}
	return out
}

// ControlPlaneMapping is the wire shape the host receives from
// the platform : "this floating IP is mapped to this VM". The
// private IP isn't known to the control plane (it lives in the
// port registry, queried per-host) — FilterToTargetSet joins
// the two.
type ControlPlaneMapping struct {
	PublicIP string
	VMName   string
}
