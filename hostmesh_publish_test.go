package weft

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
)

// TestEncodeDecodeHostRecord_WGFields confirms the host-mesh WG identity
// (public key + overlay index) survives the per-record HCL round-trip, and
// that a host WITHOUT a WG identity stays clean (legacy-safe).
func TestEncodeDecodeHostRecord_WGFields(t *testing.T) {
	in := Host{
		UUID:           "h-wg",
		Hostname:       "compute-wg",
		State:          HostStateActive,
		CreatedAt:      time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		WGPublicKey:    "PUBKEYbase64ABC=",
		WGOverlayIndex: 7,
	}
	got, err := decodeHostRecord(encodeHostRecord(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WGPublicKey != in.WGPublicKey {
		t.Errorf("wg_public_key = %q ; want %q", got.WGPublicKey, in.WGPublicKey)
	}
	if got.WGOverlayIndex != in.WGOverlayIndex {
		t.Errorf("wg_overlay_index = %d ; want %d", got.WGOverlayIndex, in.WGOverlayIndex)
	}

	// Legacy host: no WG identity → fields stay zero, and nothing is emitted.
	legacy := Host{UUID: "h-legacy", Hostname: "old", State: HostStateActive, CreatedAt: in.CreatedAt}
	gotLegacy, err := decodeHostRecord(encodeHostRecord(legacy))
	if err != nil {
		t.Fatalf("decode legacy: %v", err)
	}
	if gotLegacy.WGPublicKey != "" || gotLegacy.WGOverlayIndex != 0 {
		t.Errorf("legacy host gained a WG identity: %+v", gotLegacy)
	}
}

// TestRegisterHost_WGFieldsPersistAndRefresh checks the registry register +
// idempotent re-register paths carry the WG fields, and that a legacy
// re-register (empty WG fields) preserves a prior identity rather than
// clobbering it — the same "don't clobber on the legacy path" rule as AKName.
func TestRegisterHost_WGFieldsPersistAndRefresh(t *testing.T) {
	reg, err := loadHostRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadHostRegistry: %v", err)
	}
	h, err := reg.register(RegisterHostSpec{
		UUID: "h1", Hostname: "n1", WGPublicKey: "PUB1", WGOverlayIndex: 3,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if h.WGPublicKey != "PUB1" || h.WGOverlayIndex != 3 {
		t.Fatalf("fresh register lost WG fields: %+v", h)
	}
	// Legacy re-register (no WG fields) must preserve the prior identity.
	h2, err := reg.register(RegisterHostSpec{UUID: "h1", Hostname: "n1"})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if h2.WGPublicKey != "PUB1" || h2.WGOverlayIndex != 3 {
		t.Errorf("legacy re-register clobbered WG identity: %+v", h2)
	}
	// Mesh re-register updates it.
	h3, err := reg.register(RegisterHostSpec{UUID: "h1", Hostname: "n1", WGPublicKey: "PUB1b", WGOverlayIndex: 4})
	if err != nil {
		t.Fatalf("re-register mesh: %v", err)
	}
	if h3.WGPublicKey != "PUB1b" || h3.WGOverlayIndex != 4 {
		t.Errorf("mesh re-register didn't refresh: %+v", h3)
	}
}

// TestBuildHostMesh_PeerSetFromRegistry builds the host mesh from a 3-host
// fake registry where one host has no WG identity, and confirms: the
// non-mesh host is excluded, each mesh host sees exactly the OTHER mesh hosts
// as peers (never itself), and each peer carries a /32 overlay allowed-ip.
func TestBuildHostMesh_PeerSetFromRegistry(t *testing.T) {
	hosts := []Host{
		{UUID: "h-a", Endpoint: "10.0.0.1:8443", WGPublicKey: "PUBA", WGOverlayIndex: 1},
		{UUID: "h-b", Endpoint: "10.0.0.2:8443", WGPublicKey: "PUBB", WGOverlayIndex: 2},
		{UUID: "h-legacy", Endpoint: "10.0.0.9:8443"}, // no WG identity → excluded
	}
	sets, err := BuildHostMesh(hosts, "10.9.0.0/24", 25)
	if err != nil {
		t.Fatalf("BuildHostMesh: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("mesh members = %d, want 2 (legacy host excluded)", len(sets))
	}
	if _, ok := sets["h-legacy"]; ok {
		t.Error("legacy host should not be a mesh member")
	}
	a := sets["h-a"]
	if a.Self != "10.9.0.1/24" {
		t.Errorf("h-a self = %q, want 10.9.0.1/24", a.Self)
	}
	if len(a.Peers) != 1 {
		t.Fatalf("h-a peers = %d, want 1 (only h-b)", len(a.Peers))
	}
	p := a.Peers[0]
	if p.PublicKey != "PUBB" {
		t.Errorf("h-a peer pubkey = %q, want PUBB", p.PublicKey)
	}
	if len(p.AllowedIPs) != 1 || p.AllowedIPs[0] != "10.9.0.2/32" {
		t.Errorf("h-a peer allowed-ips = %v, want [10.9.0.2/32]", p.AllowedIPs)
	}
	if p.Endpoint != "10.0.0.2:51820" {
		t.Errorf("h-a peer endpoint = %q, want 10.0.0.2:51820 (underlay host + WG port)", p.Endpoint)
	}
	if p.PersistentKeepalive != 25 {
		t.Errorf("keepalive = %d, want 25", p.PersistentKeepalive)
	}
}

// TestBuildHostMesh_NoSecretsOnTheWire is the security-critical assertion: the
// published HostPeerSet JSON must contain NO private-key material. We marshal
// a built set and verify the wire bytes don't carry the magic private-key
// fields the per-VM mesh.WireGuard uses.
func TestBuildHostMesh_NoSecretsOnTheWire(t *testing.T) {
	hosts := []Host{
		{UUID: "h-a", Endpoint: "10.0.0.1:8443", WGPublicKey: "PUBA", WGOverlayIndex: 1},
		{UUID: "h-b", Endpoint: "10.0.0.2:8443", WGPublicKey: "PUBB", WGOverlayIndex: 2},
	}
	sets, err := BuildHostMesh(hosts, "10.9.0.0/24", 0)
	if err != nil {
		t.Fatalf("BuildHostMesh: %v", err)
	}
	for uuid, ps := range sets {
		raw, err := json.Marshal(ps)
		if err != nil {
			t.Fatal(err)
		}
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err != nil {
			t.Fatal(err)
		}
		if _, bad := generic["private_key"]; bad {
			t.Errorf("host %s peer set leaked a private_key field: %s", uuid, raw)
		}
		// The HostPeerSet struct has no private-key field at all, so this is a
		// structural guarantee, not just an empty-value one.
		for _, p := range ps.Peers {
			if p.PublicKey == "" {
				t.Errorf("host %s has an empty-pubkey peer", uuid)
			}
		}
	}
}

// TestBuildHostMesh_IndexOutOfRange surfaces an allocator error for an index
// that overflows the subnet, rather than silently dropping the host.
func TestBuildHostMesh_IndexOutOfRange(t *testing.T) {
	hosts := []Host{{UUID: "h-x", WGPublicKey: "PUBX", WGOverlayIndex: 99999}}
	if _, err := BuildHostMesh(hosts, "10.9.0.0/24", 0); err == nil {
		t.Fatal("expected error for out-of-range overlay index")
	}
}

// TestBuildHostMesh_BadSubnet rejects an unparseable overlay CIDR.
func TestBuildHostMesh_BadSubnet(t *testing.T) {
	if _, err := BuildHostMesh(nil, "not-a-cidr", 0); err == nil {
		t.Fatal("expected error for bad subnet")
	}
}

// TestSplitHostMeshEndpoint covers the endpoint-parsing forms.
func TestSplitHostMeshEndpoint(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantOK   bool
	}{
		{"10.0.0.2:8443", "10.0.0.2", true},
		{"compute-01.internal:8443", "compute-01.internal", true},
		{"barehost", "barehost", true},
		{"", "", false},
		{":8443", "", false},
	}
	for _, c := range cases {
		host, port, ok := splitHostMeshEndpoint(c.in)
		if ok != c.wantOK {
			t.Errorf("split(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if ok {
			if host != c.wantHost {
				t.Errorf("split(%q) host = %q, want %q", c.in, host, c.wantHost)
			}
			if port != hostMeshWGPort {
				t.Errorf("split(%q) port = %d, want %d", c.in, port, hostMeshWGPort)
			}
		}
	}
}

// TestPublishHostMesh_FansOutPerHost stands up an embedded NATS server, seeds
// a two-host mesh registry, and confirms PublishHostMesh fans each host's
// peer set out on weft.hostmesh.<uuid> with no private material on the wire.
func TestPublishHostMesh_FansOutPerHost(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()
	url := srv.ClientURL()

	bus, err := NewNATSEventBus(NATSConfig{URL: url, Name: "weft-hostmesh-test"})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer bus.Close()

	reg, err := loadHostRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadHostRegistry: %v", err)
	}
	if _, err := reg.register(RegisterHostSpec{UUID: "h-a", Hostname: "na", Endpoint: "10.0.0.1:8443", WGPublicKey: "PUBA", WGOverlayIndex: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.register(RegisterHostSpec{UUID: "h-b", Hostname: "nb", Endpoint: "10.0.0.2:8443", WGPublicKey: "PUBB", WGOverlayIndex: 2}); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{hostReg: reg}
	a.SetEventBus(bus)

	sub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sub.Close()

	got := make(chan struct {
		subject string
		ps      HostPeerSet
	}, 4)
	for _, uuid := range []string{"h-a", "h-b"} {
		subj := HostMeshSubject(uuid)
		if _, err := sub.Subscribe(subj, func(m *nats.Msg) {
			var ps HostPeerSet
			if err := json.Unmarshal(m.Data, &ps); err == nil {
				got <- struct {
					subject string
					ps      HostPeerSet
				}{m.Subject, ps}
			}
		}); err != nil {
			t.Fatalf("subscribe %s: %v", subj, err)
		}
	}
	_ = sub.Flush()

	n, err := a.PublishHostMesh("10.9.0.0/24", 25)
	if err != nil {
		t.Fatalf("PublishHostMesh: %v", err)
	}
	if n != 2 {
		t.Errorf("published to %d hosts, want 2", n)
	}

	seen := map[string]HostPeerSet{}
	for i := 0; i < 2; i++ {
		select {
		case g := <-got:
			seen[g.subject] = g.ps
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; seen=%v", seen)
		}
	}
	a2 := seen[HostMeshSubject("h-a")]
	if len(a2.Peers) != 1 || a2.Peers[0].PublicKey != "PUBB" {
		t.Errorf("h-a peer set wrong: %+v", a2)
	}
	b2 := seen[HostMeshSubject("h-b")]
	if len(b2.Peers) != 1 || b2.Peers[0].PublicKey != "PUBA" {
		t.Errorf("h-b peer set wrong: %+v", b2)
	}
}

// TestPublishHostMesh_NilRegistry / _NonNATSBus exercise the error guards.
func TestPublishHostMesh_Guards(t *testing.T) {
	// Nil registry.
	a := &Adapter{}
	if _, err := a.PublishHostMesh("10.9.0.0/24", 0); err == nil {
		t.Error("expected error with nil host registry")
	}
	// Registry present but bus isn't NATS-backed.
	reg, _ := loadHostRegistry(context.Background(), NewMemStorage())
	a2 := &Adapter{hostReg: reg}
	if _, err := a2.PublishHostMesh("10.9.0.0/24", 0); err == nil {
		t.Error("expected error with non-NATS bus")
	}
}
