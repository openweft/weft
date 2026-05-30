package mesh

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	"github.com/openweft/weft/wgcoord"
)

func testMembers() []Member {
	return []Member{
		{VMID: "vm1", Key: wgcoord.Key{Private: "priv1", Public: "PUB1"}, OverlayIP: netip.MustParseAddr("10.9.0.1"), Endpoint: wgcoord.Endpoint{Host: "h1", Port: 51820}, ListenPort: 51820},
		{VMID: "vm2", Key: wgcoord.Key{Private: "priv2", Public: "PUB2"}, OverlayIP: netip.MustParseAddr("10.9.0.2"), Endpoint: wgcoord.Endpoint{Host: "h2", Port: 51820}, ListenPort: 51820},
	}
}

func TestBuild_EachMemberPeersWithEveryOther(t *testing.T) {
	cfgs, err := Build(netip.MustParsePrefix("10.9.0.0/24"), testMembers(), 25)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(cfgs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(cfgs))
	}

	vm1 := cfgs["vm1"]
	if vm1.PrivateKey != "priv1" || vm1.Address != "10.9.0.1/24" {
		t.Errorf("vm1 self config wrong: %+v", vm1)
	}
	if len(vm1.Peers) != 1 || vm1.Peers[0].PublicKey != "PUB2" {
		t.Fatalf("vm1 must peer with vm2, got %+v", vm1.Peers)
	}
	if vm1.Peers[0].Endpoint != "h2:51820" {
		t.Errorf("vm1→vm2 endpoint = %s", vm1.Peers[0].Endpoint)
	}
	if vm1.Peers[0].AllowedIPs[0] != "10.9.0.2/32" {
		t.Errorf("vm1→vm2 allowed-ip = %v", vm1.Peers[0].AllowedIPs)
	}
	if vm1.Peers[0].PersistentKeepalive != 25 {
		t.Errorf("keepalive = %d", vm1.Peers[0].PersistentKeepalive)
	}
}

// TestPublish_RoundTripOverBus runs an embedded NATS server, publishes a
// member's config, and confirms a subscriber receives exactly the desired
// wg0 config — the event-bus path weft uses to notify VMs of a mesh update.
func TestPublish_RoundTripOverBus(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()

	got := make(chan pod.WireGuard, 1)
	if _, err := nc.Subscribe(Subject("vm1"), func(m *nats.Msg) {
		var wg pod.WireGuard
		if err := json.Unmarshal(m.Data, &wg); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		got <- wg
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	cfgs, err := Build(netip.MustParsePrefix("10.9.0.0/24"), testMembers(), 25)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := PublishAll(nc, cfgs); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}

	select {
	case wg := <-got:
		if wg.Address != "10.9.0.1/24" || len(wg.Peers) != 1 || wg.Peers[0].PublicKey != "PUB2" {
			t.Errorf("received unexpected config: %+v", wg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for mesh update on the bus")
	}
}
