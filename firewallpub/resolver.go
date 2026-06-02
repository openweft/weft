// Package firewallpub bridges weft's Security-Group control plane to
// the per-VM weft-microvm-agent firewall reconciler.
//
// Resolver (this file) is pure : it takes a [[Snapshot]] of the control-
// plane registries (ports, networks, security-groups) and turns it
// into a [[pod.Firewall]] for one VM — every Security-Group attached
// to one of the VM's ports is flattened, every remote_group reference
// is dereferenced to the concrete /32 (or /128) addresses of every
// port currently bound to that group.
//
// Publisher (publisher.go) wraps the resolver in a NATS publisher
// driven by the existing event bus, so a SecurityGroup edit or a
// port re-attachment causes the impacted VMs' firewalls to be
// re-published immediately.
//
// The split is deliberate : the resolver has no IO, so unit tests
// drive it with a hand-built Snapshot ; the publisher's only added
// behaviour is "which VMs need a refresh given this event", which is
// also tested separately.
package firewallpub

import (
	"net"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// Snapshot is the read-only adapter view the resolver needs. The
// production adapter satisfies it ; tests inject hand-rolled stubs.
// Kept narrow on purpose so a future migration off the adapter (e.g.
// reading directly from etcd via a different layer) only needs to
// re-implement these four methods.
type Snapshot interface {
	ListPortsForVM(vmUUID string) []weft.Port
	ListPortsForNetwork(networkUUID string) []weft.Port
	NetworkByUUID(uuid string) (weft.Network, bool)
	SecurityGroupByUUID(uuid string) (weft.SecurityGroup, bool)
}

// EffectiveFirewall computes the resolved [[pod.Firewall]] for vmUUID
// against snap. Algorithm :
//
//  1. For every port attached to vmUUID :
//     - pick the effective SG list (Port.SecurityGroups, falling back
//       to Network.DefaultSecurityGroups when the port carries none).
//     - resolve each SG to its rule list.
//     - expand every remote_group reference into one rule per
//       /32 (IPv4) or /128 (IPv6) of the other ports currently bound
//       to that SG.
//
//  2. Translate weft.SecurityRule to pod.FirewallRule (int → uint16
//     for ports, "any" → "" for protocol, drop rules whose remote_group
//     references nothing).
//
//  3. Deduplicate. The same SG can land on two ports of the VM ;
//     equal rules collapse so the nftables reconciler doesn't get
//     redundant lines.
//
// Returns a [[pod.Firewall]] whose Validate is guaranteed to succeed.
func EffectiveFirewall(snap Snapshot, vmUUID string) pod.Firewall {
	var out []pod.FirewallRule
	seen := make(map[pod.FirewallRule]struct{})

	for _, port := range snap.ListPortsForVM(vmUUID) {
		sgUUIDs := effectivePortSGs(snap, port)
		for _, sgUUID := range sgUUIDs {
			sg, ok := snap.SecurityGroupByUUID(sgUUID)
			if !ok {
				continue
			}
			for _, r := range sg.Rules {
				for _, fr := range expandRule(snap, r) {
					if _, dup := seen[fr]; dup {
						continue
					}
					seen[fr] = struct{}{}
					out = append(out, fr)
				}
			}
		}
	}
	return pod.Firewall{Rules: out}
}

// effectivePortSGs returns the SG UUID list a port resolves to,
// applying the OpenStack-equivalent fallback : empty per-port list
// → inherit the network defaults. Mirrors the rule
// validatePortSecurityGroups documents in adapter.go.
func effectivePortSGs(snap Snapshot, port weft.Port) []string {
	if len(port.SecurityGroups) > 0 {
		return port.SecurityGroups
	}
	if net, ok := snap.NetworkByUUID(port.NetworkUUID); ok {
		return net.DefaultSecurityGroups
	}
	return nil
}

// expandRule translates one weft.SecurityRule into zero, one, or
// many pod.FirewallRule entries. Many when the rule references a
// remote_group : every member port's address becomes a separate
// concrete rule (the guest never sees group references).
func expandRule(snap Snapshot, r weft.SecurityRule) []pod.FirewallRule {
	base := pod.FirewallRule{
		Direction: string(r.Direction),
		Protocol:  protocolToPod(r.Protocol),
		PortMin:   uint16(r.PortMin),
		PortMax:   uint16(r.PortMax),
	}
	if r.RemoteGroup != "" {
		var rules []pod.FirewallRule
		for _, addr := range remoteGroupCIDRs(snap, r.RemoteGroup) {
			cp := base
			cp.RemoteCIDR = addr
			rules = append(rules, cp)
		}
		return rules
	}
	base.RemoteCIDR = r.RemoteCIDR
	return []pod.FirewallRule{base}
}

// remoteGroupCIDRs returns every port-IP currently bound to sgUUID,
// each as a /32 (IPv4) or /128 (IPv6) so the nftables match is
// host-specific. Ports without an IP yet (still being provisioned)
// are skipped — they'll get included on the next publish after
// CreatePort settles their IP.
func remoteGroupCIDRs(snap Snapshot, sgUUID string) []string {
	sg, ok := snap.SecurityGroupByUUID(sgUUID)
	if !ok {
		return nil
	}
	// The SG's project might span many networks ; we need every port
	// in this project that lists sgUUID. The adapter doesn't index
	// by SG (yet), so we walk by network, which is the next-best
	// index — every port has a NetworkUUID and we already track
	// (network → ports). Ports not on any of the SG's project
	// networks can't reference this SG (validatePortSecurityGroups
	// enforces that), so this scan is correct.
	seenAddrs := make(map[string]struct{})
	var cidrs []string
	for _, n := range networksForProject(snap, sg.ProjectUUID) {
		for _, p := range snap.ListPortsForNetwork(n) {
			if p.IP == "" {
				continue
			}
			if !containsSG(p.SecurityGroups, sgUUID) {
				// Port might inherit ; check the network defaults.
				if !networkDefaultsContain(snap, p.NetworkUUID, sgUUID) {
					continue
				}
			}
			cidr := singleHostCIDR(p.IP)
			if cidr == "" {
				continue
			}
			if _, dup := seenAddrs[cidr]; dup {
				continue
			}
			seenAddrs[cidr] = struct{}{}
			cidrs = append(cidrs, cidr)
		}
	}
	return cidrs
}

// networksForProject returns the distinct network UUIDs reachable
// via ports we know about. We don't have a direct
// ListNetworksForProject on Snapshot — the resolver only needs
// network UUIDs to feed ListPortsForNetwork — so we discover them
// indirectly via NetworkByUUID misses ignored. In practice the
// publisher (which has the full adapter) supplies the network list
// via a wrapper Snapshot ; the bare-Snapshot fallback used in tests
// just walks the ports it already has, which is enough.
func networksForProject(snap Snapshot, projectUUID string) []string {
	// Snapshot doesn't expose a direct enumeration ; the production
	// wrapper in publisher.go feeds the resolver via
	// SnapshotWithNetworks (which exposes ListNetworkUUIDsForProject).
	// Plain Snapshot users get an empty list — they must rely on
	// remote_cidr rules, not remote_group, which is the testable shape.
	if e, ok := snap.(interface {
		ListNetworkUUIDsForProject(projectUUID string) []string
	}); ok {
		return e.ListNetworkUUIDsForProject(projectUUID)
	}
	return nil
}

// networkDefaultsContain returns true if the network's default SG
// list includes sgUUID.
func networkDefaultsContain(snap Snapshot, networkUUID, sgUUID string) bool {
	n, ok := snap.NetworkByUUID(networkUUID)
	if !ok {
		return false
	}
	return containsSG(n.DefaultSecurityGroups, sgUUID)
}

// containsSG is a tiny set-lookup helper kept private so future
// hot-path tuning (e.g. swapping to a map for very large SG lists)
// only edits one site.
func containsSG(list []string, uuid string) bool {
	for _, x := range list {
		if x == uuid {
			return true
		}
	}
	return false
}

// singleHostCIDR returns the /32 (IPv4) or /128 (IPv6) CIDR for ip.
// Returns "" when ip can't be parsed.
func singleHostCIDR(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() != nil {
		return parsed.To4().String() + "/32"
	}
	return parsed.String() + "/128"
}

// protocolToPod translates the weft enum to the pod string. "any"
// becomes "" so pod.FirewallRule.Validate accepts it as "any L4
// protocol" (which is what nftables encodes by omitting the
// meta l4proto match).
func protocolToPod(p weft.SecurityRuleProtocol) string {
	switch p {
	case weft.SGProtocolTCP:
		return "tcp"
	case weft.SGProtocolUDP:
		return "udp"
	case weft.SGProtocolICMP:
		return "icmp"
	default:
		// SGProtocolAny or anything we don't recognise → no L4 match.
		return ""
	}
}
