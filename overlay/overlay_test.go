package overlay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/openweft/weft/wgcoord"
)

func loadGuest(t *testing.T, path string) GuestConfig {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read guest %s: %v", path, err)
	}
	var g GuestConfig
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("unmarshal guest: %v", err)
	}
	return g
}

func TestEnsureOperatorKey_StableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	k1, err := EnsureOperatorKey(dir)
	if err != nil {
		t.Fatalf("EnsureOperatorKey: %v", err)
	}
	k2, err := EnsureOperatorKey(dir)
	if err != nil {
		t.Fatalf("EnsureOperatorKey (reload): %v", err)
	}
	if k1.Private != k2.Private || k1.Public != k2.Public {
		t.Error("operator key not stable across calls")
	}
	pub, err := wgcoord.PublicFromPrivate(k1.Private)
	if err != nil || pub != k1.Public {
		t.Errorf("operator pub mismatch: %v", err)
	}
}

func TestProvision_PairsAndWritesFiles(t *testing.T) {
	dir := t.TempDir()
	op, err := EnsureOperatorKey(dir)
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	coords, err := Provision(dir, Config{
		Subnet:       "10.9.0.0/24",
		EndpointHost: "vm-host.dc1",
		VMIndex:      3,
	}, op)
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Operator-side coords.
	if coords.Target != "10.9.0.3:51999" {
		t.Errorf("target = %s, want 10.9.0.3:51999", coords.Target)
	}
	if coords.LocalIP != "10.9.0.254" {
		t.Errorf("operator ip = %s, want 10.9.0.254 (default index)", coords.LocalIP)
	}
	if coords.PeerEndpoint != "vm-host.dc1:51820" {
		t.Errorf("peer endpoint = %s, want vm-host.dc1:51820", coords.PeerEndpoint)
	}
	if coords.PrivateKey != op.Private {
		t.Error("coords must carry the operator private key")
	}

	// Coords round-trip from disk.
	loaded, err := LoadCoords(filepath.Join(dir, CoordsFileName))
	if err != nil {
		t.Fatalf("LoadCoords: %v", err)
	}
	if loaded.Target != coords.Target || loaded.PeerPublicKey != coords.PeerPublicKey {
		t.Error("loaded coords differ from returned")
	}

	// Pairing cross-check via the on-disk guest file.
	guest := loadGuest(t, filepath.Join(dir, GuestFileName))
	vmPub, err := wgcoord.PublicFromPrivate(guest.PrivateKey)
	if err != nil {
		t.Fatalf("derive vm pub: %v", err)
	}
	if vmPub != coords.PeerPublicKey {
		t.Error("operator's peer key must equal the VM's public key")
	}
	if guest.Address != "10.9.0.3/24" {
		t.Errorf("guest address = %s, want 10.9.0.3/24", guest.Address)
	}
	if len(guest.Peers) != 1 || guest.Peers[0].PublicKey != op.Public {
		t.Errorf("guest must authorize the operator pubkey, got %+v", guest.Peers)
	}
	if len(guest.Peers[0].AllowedIPs) != 1 || guest.Peers[0].AllowedIPs[0] != "10.9.0.254/32" {
		t.Errorf("guest peer allowed-ips = %v, want [10.9.0.254/32]", guest.Peers[0].AllowedIPs)
	}
}

func TestProvision_RequiresEndpoint(t *testing.T) {
	dir := t.TempDir()
	op, _ := EnsureOperatorKey(dir)
	if _, err := Provision(dir, Config{Subnet: "10.9.0.0/24", VMIndex: 1}, op); err == nil {
		t.Error("Provision should require EndpointHost")
	}
}
