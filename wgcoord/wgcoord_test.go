package wgcoord

import (
	"net/netip"
	"testing"
)

func TestGenerateKey_RoundTrip(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if k.Private == "" || k.Public == "" {
		t.Fatal("empty key material")
	}
	derived, err := PublicFromPrivate(k.Private)
	if err != nil {
		t.Fatalf("PublicFromPrivate: %v", err)
	}
	if derived != k.Public {
		t.Errorf("derived public %q != generated %q", derived, k.Public)
	}
}

func TestGenerateKey_Distinct(t *testing.T) {
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	if a.Private == b.Private || a.Public == b.Public {
		t.Error("two GenerateKey calls produced identical material")
	}
}

func TestAllocator_HostsDistinctAndInSubnet(t *testing.T) {
	a, err := NewAllocator("10.9.0.0/24")
	if err != nil {
		t.Fatalf("NewAllocator: %v", err)
	}
	h1, err := a.Host(1)
	if err != nil {
		t.Fatalf("Host(1): %v", err)
	}
	h2, _ := a.Host(2)
	if h1.String() != "10.9.0.1" {
		t.Errorf("Host(1) = %s, want 10.9.0.1", h1)
	}
	if h2.String() != "10.9.0.2" {
		t.Errorf("Host(2) = %s, want 10.9.0.2", h2)
	}
	if !a.Prefix().Contains(h1) || !a.Prefix().Contains(h2) {
		t.Error("allocated hosts fall outside the subnet")
	}
}

func TestAllocator_Bounds(t *testing.T) {
	a, _ := NewAllocator("10.9.0.0/30") // hosts .1 and .2 usable (.0 net, .3 bcast)
	if _, err := a.Host(0); err == nil {
		t.Error("Host(0) should error (reserved network address)")
	}
	if _, err := a.Host(2); err != nil {
		t.Errorf("Host(2) should be valid in /30: %v", err)
	}
	if _, err := a.Host(99); err == nil {
		t.Error("Host(99) should overflow a /30")
	}
}

func TestPair_MatchedConfigs(t *testing.T) {
	subnet := netip.MustParsePrefix("10.9.0.0/24")
	vmKey, _ := GenerateKey()
	opKey, _ := GenerateKey()

	vm, op, err := Pair(PairInput{
		Subnet:        subnet,
		VMKey:         vmKey,
		VMIndex:       3,
		VMEndpoint:    Endpoint{Host: "vm-host.dc1", Port: 51820},
		ListenPort:    51820,
		OperatorKey:   opKey,
		OperatorIndex: 250,
		Keepalive:     25,
	})
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}

	// VM side.
	if vm.PrivateKey != vmKey.Private {
		t.Error("vm overlay must carry the VM's private key")
	}
	if vm.Address != "10.9.0.3/24" {
		t.Errorf("vm address = %s, want 10.9.0.3/24", vm.Address)
	}
	if vm.ListenPort != 51820 {
		t.Errorf("vm listen port = %d", vm.ListenPort)
	}
	if len(vm.Peers) != 1 {
		t.Fatalf("vm should have 1 peer, got %d", len(vm.Peers))
	}
	// The VM must authorize the OPERATOR's public key (not its own).
	if vm.Peers[0].PublicKey != opKey.Public {
		t.Error("vm peer must be the operator's public key")
	}
	if len(vm.Peers[0].AllowedIPs) != 1 || vm.Peers[0].AllowedIPs[0] != "10.9.0.250/32" {
		t.Errorf("vm peer allowed-ips = %v, want [10.9.0.250/32]", vm.Peers[0].AllowedIPs)
	}

	// Operator side.
	if op.PrivateKey != opKey.Private {
		t.Error("operator coords must carry the operator's private key")
	}
	if op.VMPublicKey != vmKey.Public {
		t.Error("operator must dial the VM's public key")
	}
	if op.LocalIP != "10.9.0.250" {
		t.Errorf("operator local ip = %s, want 10.9.0.250", op.LocalIP)
	}
	if op.VMOverlayIP != "10.9.0.3" {
		t.Errorf("operator's VM target = %s, want 10.9.0.3", op.VMOverlayIP)
	}
	if op.VMEndpoint != "vm-host.dc1:51820" {
		t.Errorf("operator VM endpoint = %s", op.VMEndpoint)
	}
	if len(op.AllowedIPs) != 1 || op.AllowedIPs[0] != "10.9.0.0/24" {
		t.Errorf("operator allowed-ips = %v, want [10.9.0.0/24]", op.AllowedIPs)
	}
	if op.Keepalive != 25 {
		t.Errorf("operator keepalive = %d", op.Keepalive)
	}
}

func TestMeshPeers_ExcludesSelfAndMapsAll(t *testing.T) {
	a := Member{PublicKey: "AAA", OverlayIP: netip.MustParseAddr("10.9.0.1"), Endpoint: Endpoint{Host: "h1", Port: 51820}}
	b := Member{PublicKey: "BBB", OverlayIP: netip.MustParseAddr("10.9.0.2"), Endpoint: Endpoint{Host: "h2", Port: 51820}}
	c := Member{PublicKey: "CCC", OverlayIP: netip.MustParseAddr("10.9.0.3"), Endpoint: Endpoint{Host: "h3", Port: 51820}}
	all := []Member{a, b, c}

	peers := MeshPeers(a.OverlayIP, all, 25)
	if len(peers) != 2 {
		t.Fatalf("member a should have 2 peers (b, c), got %d", len(peers))
	}
	got := map[string]PeerSpec{}
	for _, p := range peers {
		got[p.PublicKey] = p
	}
	if _, ok := got["AAA"]; ok {
		t.Error("member must not peer with itself")
	}
	pb, ok := got["BBB"]
	if !ok {
		t.Fatal("missing peer BBB")
	}
	if pb.Endpoint != "h2:51820" {
		t.Errorf("peer b endpoint = %s, want h2:51820", pb.Endpoint)
	}
	if len(pb.AllowedIPs) != 1 || pb.AllowedIPs[0] != "10.9.0.2/32" {
		t.Errorf("peer b allowed-ips = %v, want [10.9.0.2/32]", pb.AllowedIPs)
	}
	if pb.PersistentKeepalive != 25 {
		t.Errorf("peer b keepalive = %d", pb.PersistentKeepalive)
	}
}

func TestMeshPeers_SingleMemberNoPeers(t *testing.T) {
	a := Member{PublicKey: "AAA", OverlayIP: netip.MustParseAddr("10.9.0.1")}
	if peers := MeshPeers(a.OverlayIP, []Member{a}, 0); len(peers) != 0 {
		t.Errorf("lone member should have no peers, got %d", len(peers))
	}
}

func TestPair_RejectsSharedIndex(t *testing.T) {
	subnet := netip.MustParsePrefix("10.9.0.0/24")
	k, _ := GenerateKey()
	if _, _, err := Pair(PairInput{Subnet: subnet, VMKey: k, VMIndex: 5, OperatorKey: k, OperatorIndex: 5}); err == nil {
		t.Error("Pair should reject VM and operator sharing an index")
	}
}
