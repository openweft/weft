package weft

// hostmesh_publish.go is the seed-side handler behind the cluster planner's
// MeshSync action (cluster/plan.go): it (re)publishes the host-level
// WireGuard peer set after host-mesh membership changed.
//
// It is the host-mesh analogue of mesh_publish.go (which does the same for
// the per-VM overlay), with one decisive difference that is the whole point
// of cluster prereq #3's design: NO PRIVATE KEYS ARE INVOLVED. The per-VM
// mesh.Build bakes each VM's PrivateKey into the published config (the
// cleartext-over-NATS problem Part 2 addresses); the host mesh never does,
// because each host already minted its own key locally (EnsureHostWGKey) and
// registered only the public half. PublishHostMesh therefore distributes a
// pure peer set — public keys, underlay endpoints, and overlay /32s — and the
// node-agent applies it on top of its locally-held private key.
//
// Subject shape mirrors mesh.Publish's "weft.mesh.<id>": here it is
// "weft.hostmesh.<hostUUID>", one subject per host, carrying that host's full
// desired peer list (pushed wholesale, not diffed — same model as the VM
// mesh).

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"

	"github.com/openweft/weft/wgcoord"
)

// HostMeshSubject is the per-host event-bus subject the node-agent subscribes
// to for its host-mesh peer set. One subject per host UUID, mirroring
// mesh.Subject's "weft.mesh.<id>" shape.
func HostMeshSubject(hostUUID string) string { return "weft.hostmesh." + hostUUID }

// HostPeerSet is the message published on HostMeshSubject(uuid): the full
// desired host-mesh WireGuard state for one host. It carries ONLY public
// material — there is no private-key field by construction, so the wire can
// never leak a host's secret. The receiving node-agent applies these peers on
// top of the private key it holds locally (host_wg.key).
type HostPeerSet struct {
	// Self is the receiving host's own overlay address in CIDR form (the mask
	// gives it the connected route to the rest of the overlay). The agent
	// configures its wg interface address from this; its private key comes
	// from host_wg.key, never from the wire.
	Self  string         `json:"self"`
	Peers []HostMeshPeer `json:"peers"`
}

// HostMeshPeer is one WireGuard peer in a HostPeerSet — public key + underlay
// endpoint + the overlay /32 routed to it. Mirrors wgcoord.PeerSpec / the
// guest-side peer shape, kept as plain JSON fields for the wire.
type HostMeshPeer struct {
	PublicKey           string   `json:"public_key"`
	Endpoint            string   `json:"endpoint,omitempty"`
	AllowedIPs          []string `json:"allowed_ips"`
	PersistentKeepalive uint16   `json:"persistent_keepalive,omitempty"`
}

// hostMeshMember pairs a host's registry-derived mesh identity (resolved to a
// wgcoord.Member) with its UUID, so the publish loop can key each peer set by
// the right subject.
type hostMeshMember struct {
	uuid   string
	member wgcoord.Member
}

// hostMeshMembers reads the host registry and resolves the set of hosts that
// carry a host-mesh identity (non-empty WGPublicKey AND a usable overlay
// index) into wgcoord.Members. Hosts without a WG identity — every legacy /
// non-attested host — are skipped, which is exactly the implicit attestation
// gate: when --attestation-enabled, only attested nodes ever reached
// RegisterHost with a WGPublicKey, so only they appear here.
//
// Returned in UUID order for deterministic publishing/testing.
func hostMeshMembers(hosts []Host, alloc *wgcoord.Allocator) ([]hostMeshMember, error) {
	var out []hostMeshMember
	for _, h := range hosts {
		if h.WGPublicKey == "" || h.WGOverlayIndex < 1 {
			continue // no host-mesh identity → not a mesh member
		}
		ip, err := alloc.Host(h.WGOverlayIndex)
		if err != nil {
			return nil, fmt.Errorf("host %s overlay index %d: %w", h.UUID, h.WGOverlayIndex, err)
		}
		ep := wgcoord.Endpoint{}
		if host, port, ok := splitHostMeshEndpoint(h.Endpoint); ok {
			ep = wgcoord.Endpoint{Host: host, Port: port}
		}
		out = append(out, hostMeshMember{
			uuid:   h.UUID,
			member: wgcoord.Member{PublicKey: h.WGPublicKey, OverlayIP: ip, Endpoint: ep},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].uuid < out[j].uuid })
	return out, nil
}

// BuildHostMesh computes every mesh-member host's HostPeerSet from the current
// host registry. The result maps host UUID → its full desired peer set, ready
// to publish. Pure (no NATS), so the membership→peer-set logic is fully
// unit-testable from a fake registry.
//
// keepalive is the persistent-keepalive applied to every peer (helps NAT
// traversal on the underlay). subnetCIDR is the overlay subnet from
// cluster.Overlay.Subnet.
func BuildHostMesh(hosts []Host, subnetCIDR string, keepalive uint16) (map[string]HostPeerSet, error) {
	alloc, err := wgcoord.NewAllocator(subnetCIDR)
	if err != nil {
		return nil, err
	}
	members, err := hostMeshMembers(hosts, alloc)
	if err != nil {
		return nil, err
	}
	peerView := make([]wgcoord.Member, len(members))
	for i, m := range members {
		peerView[i] = m.member
	}
	bits := alloc.Prefix().Bits()
	out := make(map[string]HostPeerSet, len(members))
	for _, m := range members {
		ps := HostPeerSet{
			Self: netip.PrefixFrom(m.member.OverlayIP, bits).String(),
		}
		for _, p := range wgcoord.MeshPeers(m.member.OverlayIP, peerView, keepalive) {
			ps.Peers = append(ps.Peers, HostMeshPeer{
				PublicKey:           p.PublicKey,
				Endpoint:            p.Endpoint,
				AllowedIPs:          p.AllowedIPs,
				PersistentKeepalive: p.PersistentKeepalive,
			})
		}
		out[m.uuid] = ps
	}
	return out, nil
}

// PublishHostMesh is the seed-side handler the MeshSync action drives: it
// rebuilds the host-mesh peer set from the live host registry and fans each
// host's set out on weft.hostmesh.<uuid>. Returns the number of hosts the new
// mesh state was published to.
//
// Distribution carries public material ONLY (see HostPeerSet) — no host
// private key ever crosses the bus, so the host mesh has no cleartext-key
// exposure even though it rides the same NATS event bus as the per-VM mesh.
func (a *Adapter) PublishHostMesh(subnetCIDR string, keepalive uint16) (int, error) {
	if a.hostReg == nil {
		return 0, fmt.Errorf("host registry not initialised")
	}
	nc, err := natsConnFromBus(a.bus)
	if err != nil {
		return 0, err
	}
	sets, err := BuildHostMesh(a.hostReg.list(), subnetCIDR, keepalive)
	if err != nil {
		return 0, err
	}
	for uuid, ps := range sets {
		data, err := json.Marshal(ps)
		if err != nil {
			return 0, err
		}
		if err := nc.Publish(HostMeshSubject(uuid), data); err != nil {
			return 0, fmt.Errorf("publish host mesh %s: %w", uuid, err)
		}
	}
	if err := nc.Flush(); err != nil {
		return 0, err
	}
	return len(sets), nil
}

// splitHostMeshEndpoint parses a host registry Endpoint ("host:port") into its
// parts for the underlay reachability of a host's WG listener. weft's host
// Endpoint is the agent's gRPC "host:port"; the WG underlay shares the host
// part and the wg listen port is the conventional overlay.DefaultListenPort.
// Returns ok=false when the endpoint has no parseable host (the peer is then
// published endpoint-less and learned from its first handshake).
func splitHostMeshEndpoint(endpoint string) (host string, port uint16, ok bool) {
	if endpoint == "" {
		return "", 0, false
	}
	ap, err := netip.ParseAddrPort(endpoint)
	if err == nil {
		return ap.Addr().String(), hostMeshWGPort, true
	}
	// Non-AddrPort form (e.g. "name:8443"): take the host before the last
	// colon, ignore the gRPC port, use the conventional WG underlay port.
	for i := len(endpoint) - 1; i >= 0; i-- {
		if endpoint[i] == ':' {
			if i == 0 {
				return "", 0, false
			}
			return endpoint[:i], hostMeshWGPort, true
		}
	}
	// Bare host, no port at all.
	return endpoint, hostMeshWGPort, true
}

// hostMeshWGPort is the conventional underlay UDP port a host's host-mesh
// WireGuard listener binds — shared with the per-VM overlay default so a host
// runs a single wg listener. Mirrors overlay.DefaultListenPort (kept as a
// local const to avoid importing the overlay package here).
const hostMeshWGPort = uint16(51820)
