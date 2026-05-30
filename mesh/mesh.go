// Package mesh is weft's side of dynamic WireGuard mesh updates. It computes
// each VM's full desired wg0 config from the current mesh membership and
// publishes it on the event bus; the in-VM agent (weft-microvm-agent/pkg/mesh)
// subscribes and re-applies. State is pushed whole, so adding or removing a
// VM is just "recompute every member's config and re-publish".
package mesh

import (
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/nats-io/nats.go"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	"github.com/openweft/weft/wgcoord"
)

// Subject is the per-VM event-bus subject. Must match
// weft-microvm-agent/pkg/mesh.Subject (both pin "weft.mesh.<id>" in tests).
func Subject(vmID string) string { return "weft.mesh." + vmID }

// Member is one VM on the overlay mesh, as weft knows it: its id, its keypair
// (weft minted both halves at provision time), its overlay address, and the
// underlay endpoint peers reach it at.
type Member struct {
	VMID       string
	Key        wgcoord.Key
	OverlayIP  netip.Addr
	Endpoint   wgcoord.Endpoint
	Interface  string // default "wg0"
	ListenPort uint16
}

// Build computes every member's full desired wg0 config: its own key and
// address, plus a peer for every other member. The result maps VM id → config
// ready to publish.
func Build(subnet netip.Prefix, members []Member, keepalive uint16) (map[string]pod.WireGuard, error) {
	if !subnet.IsValid() {
		return nil, fmt.Errorf("invalid subnet")
	}
	bits := subnet.Bits()

	peerView := make([]wgcoord.Member, len(members))
	for i, m := range members {
		peerView[i] = wgcoord.Member{PublicKey: m.Key.Public, OverlayIP: m.OverlayIP, Endpoint: m.Endpoint}
	}

	out := make(map[string]pod.WireGuard, len(members))
	for _, m := range members {
		iface := m.Interface
		if iface == "" {
			iface = "wg0"
		}
		cfg := pod.WireGuard{
			Interface:  iface,
			PrivateKey: m.Key.Private,
			ListenPort: m.ListenPort,
			Address:    netip.PrefixFrom(m.OverlayIP, bits).String(),
		}
		for _, p := range wgcoord.MeshPeers(m.OverlayIP, peerView, keepalive) {
			cfg.Peers = append(cfg.Peers, pod.WGPeer{
				PublicKey:           p.PublicKey,
				Endpoint:            p.Endpoint,
				AllowedIPs:          p.AllowedIPs,
				PersistentKeepalive: p.PersistentKeepalive,
			})
		}
		out[m.VMID] = cfg
	}
	return out, nil
}

// Publish sends one VM's desired config on its mesh subject.
func Publish(nc *nats.Conn, vmID string, cfg pod.WireGuard) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return nc.Publish(Subject(vmID), data)
}

// PublishAll publishes every member's recomputed config — the call weft makes
// after a membership change to refresh the whole mesh.
func PublishAll(nc *nats.Conn, configs map[string]pod.WireGuard) error {
	for vmID, cfg := range configs {
		if err := Publish(nc, vmID, cfg); err != nil {
			return fmt.Errorf("publish %s: %w", vmID, err)
		}
	}
	return nc.Flush()
}
