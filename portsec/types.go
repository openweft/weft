// Package portsec is the host-side anti-spoofing reconciler.
// Drops any frame leaving the host through a VM's tap interface
// whose source MAC + IP doesn't match the Port the VM was created
// with. Prevents a compromised tenant from impersonating another
// tenant's MAC or IP on the shared L2 segment.
//
// Architecture :
//
//	weft (control plane)  ─ port.{created,security_groups_updated,deleted}
//	                                       ▼
//	portsec.Watcher       ─ ComputeLocalRules
//	                                       ▼
//	LinuxReconciler.Apply ─ nftables `bridge weft-portsec` table
//	                                       ▼
//	per-tap chain : drop frames with src MAC != expected
//	                drop frames with src IPv4/IPv6 != expected
//	                accept everything else
//
// Bridge-family nftables is the natural fit : it hooks at L2 just
// after the bridge sees the frame, so we can match on Ethernet
// fields (saddr) AND payload IP fields. The same chain can carry
// both v4 and v6 rules.
//
// One reconciler per host. Replaces the whole "weft-portsec" table
// on every Apply ; stale rules disappear automatically when a Port
// is deleted from the input set.
//
// Tap-interface discovery (which kernel interface backs which VM /
// Port) is the hypervisor driver's responsibility ; this package
// takes the (tap-ifname, MAC, IPs) tuples as input and trusts the
// caller. weft-driver-vz / weft-driver-qemu populate that via the
// existing per-VM event sink.
package portsec

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// AntispoofRule is one tap interface's allow-set. The reconciler
// installs nftables rules that drop anything not matching MAC +
// at least one of IPs. Multiple IPs allow a port to legitimately
// carry secondary addresses (rare today but reserved for future
// multi-IP-per-port plumbing).
type AntispoofRule struct {
	// TapInterface is the host-side kernel interface name backing
	// the VM's NIC (e.g. "tap-abc123", "vnet0"). Required.
	TapInterface string
	// MAC is the only Ethernet source address the port is allowed
	// to emit. Required.
	MAC string
	// IPs is the set of allowed source IPs (IPv4 or IPv6). Empty
	// IPs disables IP-level filtering, MAC-only — useful for
	// pre-DHCP boot where the VM hasn't claimed its IP yet.
	IPs []string
	// VMName is informational, included in the nftables rule
	// comment so `nft -a list ruleset` shows which tenant.
	VMName string
}

// Validate checks one rule's invariants. Used by Apply to fail
// fast on malformed input rather than after a partial netlink
// commit.
func (r AntispoofRule) Validate() error {
	if r.TapInterface == "" {
		return fmt.Errorf("empty tap_interface")
	}
	if len(r.TapInterface) > 15 {
		// Linux IFNAMSIZ = 16 (15 chars + NUL). Bigger names
		// can't reach the kernel.
		return fmt.Errorf("tap_interface %q too long (>15 chars)", r.TapInterface)
	}
	if r.MAC == "" {
		return fmt.Errorf("empty mac")
	}
	if !looksLikeMAC(r.MAC) {
		return fmt.Errorf("mac %q: malformed", r.MAC)
	}
	for i, ip := range r.IPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			return fmt.Errorf("ip[%d] %q: %w", i, ip, err)
		}
	}
	return nil
}

// looksLikeMAC checks for the dotted (or colon) hex form. Not a
// full RFC 5342 validator ; rejects obvious garbage.
func looksLikeMAC(mac string) bool {
	sep := ":"
	if strings.Contains(mac, "-") {
		sep = "-"
	}
	parts := strings.Split(mac, sep)
	if len(parts) != 6 {
		return false
	}
	for _, p := range parts {
		if len(p) != 2 {
			return false
		}
		for _, c := range p {
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// Reconciler is the platform abstraction over the netlink-or-stub
// backend. Apply is whole-state : the kernel bridge table owned
// by this package is replaced atomically on every call.
type Reconciler interface {
	Apply(rules []AntispoofRule) error
}

// ValidateRules checks every rule + rejects duplicate tap
// interfaces (one rule per tap, kernel-level constraint). Pulled
// out of Apply so the caller sees clear errors pre-commit.
func ValidateRules(rules []AntispoofRule) error {
	seenTap := make(map[string]struct{})
	seenMAC := make(map[string]struct{})
	for i, r := range rules {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("rule[%d]: %w", i, err)
		}
		if _, dup := seenTap[r.TapInterface]; dup {
			return fmt.Errorf("rule[%d]: tap %q appears twice", i, r.TapInterface)
		}
		seenTap[r.TapInterface] = struct{}{}
		mac := strings.ToLower(r.MAC)
		if _, dup := seenMAC[mac]; dup {
			return fmt.Errorf("rule[%d]: mac %s appears twice (cross-tenant collision risk)", i, mac)
		}
		seenMAC[mac] = struct{}{}
	}
	return nil
}

// SortRules returns a copy of rules sorted by TapInterface so the
// nftables output is byte-stable across reconciles for equivalent
// input. Easier diffing for operators inspecting `nft list table`.
func SortRules(rules []AntispoofRule) []AntispoofRule {
	out := make([]AntispoofRule, len(rules))
	copy(out, rules)
	sort.Slice(out, func(i, j int) bool { return out[i].TapInterface < out[j].TapInterface })
	return out
}

// TableName is the bridge-family nftables table the reconciler
// owns. Exported so operator tooling + tests can address it.
const TableName = "weft-portsec"
