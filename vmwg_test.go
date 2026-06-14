package weft

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/openweft/weft/wgcoord"
)

// TestEnsureVMWGKey_StableAndPrivatePersisted mirrors the host-key test: the
// public key is stable across calls and the PRIVATE key (not the returned
// public one) is what lands on disk at 0o600.
func TestEnsureVMWGKey_StableAndPrivatePersisted(t *testing.T) {
	dir := t.TempDir()
	pub1, err := EnsureVMWGKey(dir)
	if err != nil {
		t.Fatalf("EnsureVMWGKey: %v", err)
	}
	pub2, err := EnsureVMWGKey(dir)
	if err != nil {
		t.Fatalf("EnsureVMWGKey reload: %v", err)
	}
	if pub1 != pub2 || pub1 == "" {
		t.Fatalf("vm pubkey unstable/empty: %q vs %q", pub1, pub2)
	}
	path := filepath.Join(dir, vmWGKeyFile)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("vm key perm = %o, want 600", fi.Mode().Perm())
	}
	priv, _ := os.ReadFile(path)
	derived, err := wgcoord.PublicFromPrivate(string(priv))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if derived != pub1 {
		t.Error("persisted material is not the private key for the returned public key")
	}
	if string(priv) == pub1 {
		t.Error("returned key equals persisted private key — private half must never be returned")
	}
}

// TestEnsureVMWGKey_Corrupt surfaces a decode error.
func TestEnsureVMWGKey_Corrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, vmWGKeyFile), []byte("@@@"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureVMWGKey(dir); err == nil {
		t.Fatal("expected decode error for corrupt vm key")
	}
}

// TestBuildVMMesh_PubkeyOnly_NoPrivateOnWire is the security-critical test for
// Part 2: the published VM peer set carries PUBLIC material only. The
// VMMeshMember input itself has no private-key field, and the serialised
// VMPeerSet must contain no private_key.
func TestBuildVMMesh_PubkeyOnly_NoPrivateOnWire(t *testing.T) {
	subnet := netip.MustParsePrefix("10.9.0.0/24")
	members := []VMMeshMember{
		{VMID: "vm1", PublicKey: "PUB1", OverlayIP: netip.MustParseAddr("10.9.0.1"), Endpoint: wgcoord.Endpoint{Host: "h1", Port: 51820}},
		{VMID: "vm2", PublicKey: "PUB2", OverlayIP: netip.MustParseAddr("10.9.0.2"), Endpoint: wgcoord.Endpoint{Host: "h2", Port: 51820}},
	}
	sets, err := BuildVMMesh(subnet, members, 25)
	if err != nil {
		t.Fatalf("BuildVMMesh: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("sets = %d, want 2", len(sets))
	}
	v1 := sets["vm1"]
	if v1.Self != "10.9.0.1/24" {
		t.Errorf("vm1 self = %q", v1.Self)
	}
	if len(v1.Peers) != 1 || v1.Peers[0].PublicKey != "PUB2" {
		t.Errorf("vm1 peers wrong: %+v", v1.Peers)
	}
	if v1.Peers[0].AllowedIPs[0] != "10.9.0.2/32" {
		t.Errorf("vm1 peer allowed-ip = %v", v1.Peers[0].AllowedIPs)
	}
	for id, ps := range sets {
		raw, _ := json.Marshal(ps)
		var g map[string]any
		_ = json.Unmarshal(raw, &g)
		if _, bad := g["private_key"]; bad {
			t.Errorf("vm %s peer set leaked private_key: %s", id, raw)
		}
	}
}

// TestBuildVMMesh_BadSubnet rejects an invalid subnet.
func TestBuildVMMesh_BadSubnet(t *testing.T) {
	if _, err := BuildVMMesh(netip.Prefix{}, nil, 0); err == nil {
		t.Fatal("expected error for invalid subnet")
	}
}

// TestPublishVMMesh_FansOutOnMeshSubject confirms the secret-free path reuses
// weft.mesh.<vmID> and delivers pubkey-only peer sets.
func TestPublishVMMesh_FansOutOnMeshSubject(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()
	url := srv.ClientURL()

	bus, err := NewNATSEventBus(NATSConfig{URL: url, Name: "weft-vmmesh-test"})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer bus.Close()

	a := &Adapter{}
	a.SetEventBus(bus)

	members := []VMMeshMember{
		{VMID: "vm1", PublicKey: "PUB1", OverlayIP: netip.MustParseAddr("10.9.0.1"), Endpoint: wgcoord.Endpoint{Host: "h1", Port: 51820}},
		{VMID: "vm2", PublicKey: "PUB2", OverlayIP: netip.MustParseAddr("10.9.0.2"), Endpoint: wgcoord.Endpoint{Host: "h2", Port: 51820}},
	}

	sub, err := nats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	got := make(chan VMPeerSet, 4)
	for _, vmID := range []string{"vm1", "vm2"} {
		if _, err := sub.Subscribe("weft.mesh."+vmID, func(m *nats.Msg) {
			var ps VMPeerSet
			if err := json.Unmarshal(m.Data, &ps); err == nil {
				got <- ps
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	_ = sub.Flush()

	n, err := a.PublishVMMesh(members, netip.MustParsePrefix("10.9.0.0/24"), 25)
	if err != nil {
		t.Fatalf("PublishVMMesh: %v", err)
	}
	if n != 2 {
		t.Errorf("published = %d, want 2", n)
	}
	for i := 0; i < 2; i++ {
		select {
		case ps := <-got:
			if len(ps.Peers) != 1 {
				t.Errorf("want 1 peer, got %d", len(ps.Peers))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for VM mesh publish")
		}
	}
}

// TestPublishVMMesh_NonNATSBus exercises the error guard.
func TestPublishVMMesh_NonNATSBus(t *testing.T) {
	a := &Adapter{}
	if _, err := a.PublishVMMesh(nil, netip.MustParsePrefix("10.9.0.0/24"), 0); err == nil {
		t.Error("expected error with non-NATS bus")
	}
}
