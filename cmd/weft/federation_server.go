package main

// federation_server.go owns the server-side Federation RPCs. The
// in-process `federation.Poller` already maintains a snapshot of every
// configured peer's last successful /cluster-info poll ; this file
// exposes that snapshot over gRPC so weft-webui (and other clients)
// can render the federation page without re-polling.
//
// Per [[openweft_pull_model]] the RPC is a read of the locally-cached
// pull state — no remote pull happens on the hot path.

import (
	"context"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/federation"
)

// ListFederationPeers returns the cached snapshot. When the poller
// isn't configured (no `federation { peers = [...] }` block in
// weft.hcl, or single-cluster mode), the response carries an empty
// peers slice — clients render "no federation" rather than erroring.
func (s *weftServer) ListFederationPeers(_ context.Context, _ *weftv1.ListFederationPeersRequest) (*weftv1.ListFederationPeersResponse, error) {
	if s.federationPoller == nil {
		return &weftv1.ListFederationPeersResponse{}, nil
	}
	snap := s.federationPoller.Snapshot()
	out := &weftv1.ListFederationPeersResponse{
		Peers: make([]*weftv1.FederationPeerInfo, 0, len(snap)),
	}
	for _, p := range snap {
		row := &weftv1.FederationPeerInfo{
			Name:      p.Name,
			Url:       p.URL,
			Status:    p.Status,
			LastError: p.LastError,
		}
		if !p.LastSeen.IsZero() {
			row.LastSeenUnixNs = p.LastSeen.UnixNano()
		}
		// Region + weight come from the peer's manifest entry, when
		// it matches this peer's URL. Best-effort — empty when the
		// poller hasn't yet seen a manifest, or when no member's
		// public_endpoints list points at our peer URL.
		if p.Manifest != nil {
			if m := pickManifestMember(p.Manifest, p.URL); m != nil {
				row.Region = m.Region
				row.Weight = int32(m.Weight)
			}
		}
		out.Peers = append(out.Peers, row)
	}
	return out, nil
}

// pickManifestMember returns the Cluster in m whose PublicEndpoints
// include peerURL (or its base form). Returns nil when no match —
// federation.matchMemberByEndpoint is package-private so we duplicate
// the trivial walk here rather than expose the helper.
func pickManifestMember(m *federation.FederationManifest, peerURL string) *federation.Cluster {
	if m == nil {
		return nil
	}
	for i := range m.Members {
		for _, ep := range m.Members[i].PublicEndpoints {
			if ep == peerURL {
				return &m.Members[i]
			}
		}
	}
	return nil
}
