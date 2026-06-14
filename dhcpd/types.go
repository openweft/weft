// Package dhcpd is the host-side DHCPv4 server for openweft
// networks declared `type = "bridged"` or any other shape that
// expects guests to acquire their IP / gateway / DNS via DHCP
// rather than via the in-VM WireGuard config or static cidata.
//
// Why a built-in server : in academic / enterprise environments
// where the establishment provides a VLAN trunk + subnet but no
// DHCP service, openweft needs to hand out lease information for
// each VM it spawns on the bridged network. Running an external
// dnsmasq adds a process to manage ; a small pure-Go server
// embedded in weft-agent keeps the platform's "one daemon"
// shape.
//
// Architecture (when fully wired) :
//
//	weft.Port (MAC, IP, NetworkUUID)
//	                ▼
//	weft.Network (CIDR, Gateway, DNSServers, lease time defaults)
//	                ▼
//	dhcpd.Source ─ Resolve(mac) → Lease{IP, Gateway, DNS, ...}
//	                ▼
//	dhcpd.Server listens on the bridge interface UDP :67
//	             handles DISCOVER/OFFER/REQUEST/ACK
//	             replies via broadcast to chaddr
//
// THIS FILE : just the types + Source interface + ServerOptions.
// The DHCPv4 protocol implementation lands in server_linux.go when
// either (a) the weft module's broken transitive dep
// (go-compressions/matchlen) is cleared so `insomniacslk/dhcp`
// can be added via go.mod, or (b) we ship a hand-rolled
// pure-Go DHCPv4 implementation. Both options are documented as
// follow-up — this slice ships the foundation so the integration
// surface is stable.
package dhcpd

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// Lease is the per-MAC response the server hands out. Fields
// mirror the standard DHCPv4 options the server emits :
//
//   - Yiaddr      → option type 1 (subnet mask is derived from
//                   the Lease's prefix length)
//   - Router      → option 3
//   - DNSServers  → option 6
//   - Domain      → option 15 (optional)
//   - LeaseTime   → option 51 (defaults to 1h when zero)
//
// The lease is computed by the caller's Source on each request ;
// the server doesn't persist anything itself.
type Lease struct {
	// Yiaddr is the address handed to the client.
	Yiaddr netip.Addr
	// SubnetMaskBits is the prefix length (e.g. 24 for /24)
	// from which the server derives the netmask option.
	SubnetMaskBits int
	// Router is the default gateway. Optional ; zero value
	// skips option 3.
	Router netip.Addr
	// DNSServers is the list of resolvers. Empty skips option 6.
	DNSServers []netip.Addr
	// Domain is the search domain. Empty skips option 15.
	Domain string
	// LeaseTime is the lease validity. Zero = 1h default.
	LeaseTime time.Duration
}

// Validate enforces the minimum invariants the server needs to
// build a well-formed reply.
func (l Lease) Validate() error {
	if !l.Yiaddr.IsValid() {
		return errors.New("lease: yiaddr is required")
	}
	if !l.Yiaddr.Is4() {
		return errors.New("lease: yiaddr must be IPv4 (this is a DHCPv4 server)")
	}
	if l.SubnetMaskBits <= 0 || l.SubnetMaskBits > 32 {
		return fmt.Errorf("lease: subnet_mask_bits out of range (1-32): %d", l.SubnetMaskBits)
	}
	if l.Router.IsValid() && !l.Router.Is4() {
		return errors.New("lease: router must be IPv4")
	}
	for i, ns := range l.DNSServers {
		if !ns.IsValid() || !ns.Is4() {
			return fmt.Errorf("lease: dns[%d] must be a valid IPv4", i)
		}
	}
	if l.LeaseTime < 0 {
		return fmt.Errorf("lease: lease_time must be ≥ 0 : %s", l.LeaseTime)
	}
	return nil
}

// Source resolves a client MAC into a Lease. Returns
// (Lease{}, false) when no lease should be issued (unknown MAC
// → silently dropped, no NAK).
//
// Implementations bridge the server to weft's Port + Network
// registries : look up the Port by MAC, find its Network, build
// the Lease from the Network's CIDR + Gateway + DNSServers.
type Source interface {
	Resolve(mac string) (Lease, bool)
}

// SourceFn is a function-type Source for tests + small adapters.
type SourceFn func(mac string) (Lease, bool)

// Resolve calls the function.
func (f SourceFn) Resolve(mac string) (Lease, bool) { return f(mac) }

// Server is the public surface. Run blocks until ctx is cancelled.
// The concrete implementation lives in server_linux.go (real
// UDP/67 + protocol) and server_other.go (no-op stub).
type Server interface {
	Run(ctx contextLike) error
}

// contextLike is the narrow context surface the Server uses.
// Pulled into a tiny interface so test stubs don't need to
// import context (and so this types file stays import-light).
type contextLike interface {
	Done() <-chan struct{}
	Err() error
}

// Options is the constructor input for both real + stub servers.
type Options struct {
	// Interface is the host-side kernel interface the server
	// binds to (e.g. "br0", "eth0.100"). Required ; the server
	// uses SO_BINDTODEVICE so it only sees broadcast traffic
	// from this VLAN / bridge.
	Interface string
	// ServerIP is the address the server announces as option 54
	// (server identifier). Conventionally the host's address
	// on the bridged network. Required.
	ServerIP netip.Addr
	// Source resolves the client MAC into a Lease. Required.
	Source Source
}

// Validate checks the cross-field invariants.
func (o Options) Validate() error {
	if o.Interface == "" {
		return errors.New("dhcpd: empty Interface")
	}
	if !o.ServerIP.IsValid() || !o.ServerIP.Is4() {
		return errors.New("dhcpd: ServerIP must be a valid IPv4")
	}
	if o.Source == nil {
		return errors.New("dhcpd: nil Source")
	}
	return nil
}
