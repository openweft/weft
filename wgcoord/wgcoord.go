// Package wgcoord is vzd's WireGuard overlay coordinator: it mints
// per-VM (and per-operator) Curve25519 keypairs, allocates overlay IPs
// from a subnet, and pairs the two sides into ready-to-apply configs —
// the VM-side block baked into the pod spec, and the operator-side
// coordinates the CLI dials with.
//
// It is pure (stdlib + x/crypto only) and carries no kernel or network
// dependency, so the allocation and pairing logic is fully unit-testable.
package wgcoord

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/netip"

	"golang.org/x/crypto/curve25519"
)

const keyLen = 32

// Key is a base64-encoded Curve25519 keypair, matching `wg`'s textual form.
type Key struct {
	Private string
	Public  string
}

// GenerateKey mints a fresh clamped Curve25519 keypair.
func GenerateKey() (Key, error) {
	priv := make([]byte, keyLen)
	if _, err := rand.Read(priv); err != nil {
		return Key{}, err
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return Key{}, err
	}
	return Key{
		Private: base64.StdEncoding.EncodeToString(priv),
		Public:  base64.StdEncoding.EncodeToString(pub),
	}, nil
}

// PublicFromPrivate derives the base64 public key from a base64 private key.
func PublicFromPrivate(privB64 string) (string, error) {
	priv, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		return "", fmt.Errorf("decode private key: %w", err)
	}
	if len(priv) != keyLen {
		return "", fmt.Errorf("private key must be %d bytes, got %d", keyLen, len(priv))
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

// Allocator hands out host addresses from an overlay subnet. Allocation is
// deterministic by index, so a VM's UUID-derived index always maps to the
// same overlay IP across restarts without persisting the IP separately.
type Allocator struct {
	subnet netip.Prefix
}

// NewAllocator builds an allocator for the given overlay CIDR (e.g.
// "10.9.0.0/24").
func NewAllocator(cidr string) (*Allocator, error) {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("overlay subnet %q: %w", cidr, err)
	}
	return &Allocator{subnet: p.Masked()}, nil
}

// Prefix returns the (masked) overlay subnet.
func (a *Allocator) Prefix() netip.Prefix { return a.subnet }

// Host returns the n-th usable host in the subnet (n >= 1; n = 0 is the
// network address and is reserved). It errors if n falls outside the subnet.
func (a *Allocator) Host(n int) (netip.Addr, error) {
	if n < 1 {
		return netip.Addr{}, fmt.Errorf("host index must be >= 1, got %d", n)
	}
	addr := a.subnet.Addr()
	for i := 0; i < n; i++ {
		addr = addr.Next()
		if !addr.IsValid() {
			return netip.Addr{}, fmt.Errorf("host index %d overflows address space", n)
		}
	}
	if !a.subnet.Contains(addr) {
		return netip.Addr{}, fmt.Errorf("host index %d outside subnet %s", n, a.subnet)
	}
	return addr, nil
}

// Endpoint is the underlay (real-network) reachability of a VM's WireGuard
// listener — the address an operator's WireGuard sends encrypted UDP to.
type Endpoint struct {
	Host string
	Port uint16
}

func (e Endpoint) String() string { return fmt.Sprintf("%s:%d", e.Host, e.Port) }

// VMOverlay is the WireGuard block to bake into a VM's pod spec
// (mirrors weft-microvm-init/pkg/pod.WireGuard; kept as plain fields so this
// package stays free of the guest-side import).
type VMOverlay struct {
	Interface  string
	PrivateKey string
	Address    string // overlay CIDR (carries the subnet mask → connected route)
	ListenPort uint16
	Peers      []PeerSpec
}

type PeerSpec struct {
	PublicKey           string
	AllowedIPs          []string
	Endpoint            string
	PersistentKeepalive uint16
}

// OperatorCoords is everything the CLI needs to dial a VM's agent over the
// overlay (maps directly onto grpc-transports/wireguard ClientConfig).
type OperatorCoords struct {
	PrivateKey  string   // operator's private key
	LocalIP     string   // operator's overlay IP (bare, no mask)
	VMPublicKey string   // the VM's public key
	VMEndpoint  string   // the VM's underlay "host:port"
	VMOverlayIP string   // the VM's overlay IP — the agent's listen host
	AllowedIPs  []string // overlay prefixes routed to the VM
	Keepalive   uint16
}

// PairInput describes one VM↔operator pairing to coordinate.
type PairInput struct {
	Subnet        netip.Prefix
	Interface     string   // VM-side wg interface name (default "wg0")
	VMKey         Key
	VMIndex       int      // host index of the VM in the subnet
	VMEndpoint    Endpoint // the VM's underlay reachability
	ListenPort    uint16   // the VM's wg listen port
	OperatorKey   Key
	OperatorIndex int    // host index of the operator in the subnet
	Keepalive     uint16 // operator→VM keepalive (helps traverse NAT)
}

// Pair turns one PairInput into the matched VM-side and operator-side
// configs: the VM authorizes the operator's key (allowed-ip = operator /32),
// and the operator gets the VM's key + underlay endpoint + overlay target.
func Pair(in PairInput) (VMOverlay, OperatorCoords, error) {
	if !in.Subnet.IsValid() {
		return VMOverlay{}, OperatorCoords{}, fmt.Errorf("invalid subnet")
	}
	bits := in.Subnet.Bits()
	a := &Allocator{subnet: in.Subnet.Masked()}

	vmIP, err := a.Host(in.VMIndex)
	if err != nil {
		return VMOverlay{}, OperatorCoords{}, fmt.Errorf("vm ip: %w", err)
	}
	opIP, err := a.Host(in.OperatorIndex)
	if err != nil {
		return VMOverlay{}, OperatorCoords{}, fmt.Errorf("operator ip: %w", err)
	}
	if vmIP == opIP {
		return VMOverlay{}, OperatorCoords{}, fmt.Errorf("vm and operator share index %d", in.VMIndex)
	}

	iface := in.Interface
	if iface == "" {
		iface = "wg0"
	}

	vm := VMOverlay{
		Interface:  iface,
		PrivateKey: in.VMKey.Private,
		Address:    netip.PrefixFrom(vmIP, bits).String(), // mask → connected route reaches the operator
		ListenPort: in.ListenPort,
		Peers: []PeerSpec{{
			PublicKey:  in.OperatorKey.Public,
			AllowedIPs: []string{hostPrefix(opIP)},
			// Operator endpoint is left empty: it roams / sits behind NAT and
			// is learned from its first handshake.
		}},
	}

	op := OperatorCoords{
		PrivateKey:  in.OperatorKey.Private,
		LocalIP:     opIP.String(),
		VMPublicKey: in.VMKey.Public,
		VMEndpoint:  in.VMEndpoint.String(),
		VMOverlayIP: vmIP.String(),
		AllowedIPs:  []string{in.Subnet.Masked().String()}, // route the whole overlay to the VM
		Keepalive:   in.Keepalive,
	}

	return vm, op, nil
}

// hostPrefix renders a single-host prefix (/32 or /128) for an address.
func hostPrefix(a netip.Addr) string {
	return netip.PrefixFrom(a, a.BitLen()).String()
}

// Member is one node on the overlay mesh, as seen by its peers: its public
// key, its overlay address, and the underlay endpoint peers dial to reach it.
type Member struct {
	PublicKey string
	OverlayIP netip.Addr
	Endpoint  Endpoint
}

// MeshPeers returns the WireGuard peer set for the member identified by
// selfIP: every other member, each with its public key, its underlay
// endpoint, and a /32 allowed-ip for its overlay address. This is the
// full-state peer list a VM applies on a mesh update — recomputed and pushed
// wholesale rather than diffed.
func MeshPeers(selfIP netip.Addr, all []Member, keepalive uint16) []PeerSpec {
	peers := make([]PeerSpec, 0, len(all))
	for _, m := range all {
		if m.OverlayIP == selfIP {
			continue // don't peer with yourself
		}
		peers = append(peers, PeerSpec{
			PublicKey:           m.PublicKey,
			AllowedIPs:          []string{hostPrefix(m.OverlayIP)},
			Endpoint:            m.Endpoint.String(),
			PersistentKeepalive: keepalive,
		})
	}
	return peers
}
