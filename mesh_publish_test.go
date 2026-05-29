package weft

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/test"
	"github.com/nats-io/nats.go"
	"github.com/openweft/weft-microvm-init/pkg/pod"
	"github.com/openweft/weft/mesh"
	"github.com/openweft/weft/wgcoord"
)

// TestPublishMesh_FansOutPerVM stands up an embedded NATS server, asks
// PublishMesh to recompute a two-member mesh, and confirms each VM receives
// a config naming itself in PrivateKey + the other as the sole peer.
func TestPublishMesh_FansOutPerVM(t *testing.T) {
	srv := natsserver.RunRandClientPortServer()
	defer srv.Shutdown()
	url := srv.ClientURL()

	bus, err := NewNATSEventBus(NATSConfig{URL: url, Name: "weft-test"})
	if err != nil {
		t.Fatalf("NewNATSEventBus: %v", err)
	}
	defer bus.Close()

	a := &Adapter{}
	a.SetEventBus(bus)

	members := []mesh.Member{
		{VMID: "vm1", Key: wgcoord.Key{Private: "priv1", Public: "PUB1"},
			OverlayIP: netip.MustParseAddr("10.9.0.1"),
			Endpoint:  wgcoord.Endpoint{Host: "h1", Port: 51820}, ListenPort: 51820},
		{VMID: "vm2", Key: wgcoord.Key{Private: "priv2", Public: "PUB2"},
			OverlayIP: netip.MustParseAddr("10.9.0.2"),
			Endpoint:  wgcoord.Endpoint{Host: "h2", Port: 51820}, ListenPort: 51820},
	}

	sub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sub.Close()

	got := make(chan pod.WireGuard, 4)
	for _, vmID := range []string{"vm1", "vm2"} {
		if _, err := sub.Subscribe("weft.mesh."+vmID, func(m *nats.Msg) {
			var wg pod.WireGuard
			if err := json.Unmarshal(m.Data, &wg); err == nil {
				got <- wg
			}
		}); err != nil {
			t.Fatalf("subscribe %s: %v", vmID, err)
		}
	}
	_ = sub.Flush()

	n, err := a.PublishMesh(members, netip.MustParsePrefix("10.9.0.0/24"), 25)
	if err != nil {
		t.Fatalf("PublishMesh: %v", err)
	}
	if n != 2 {
		t.Errorf("vm_count = %d, want 2", n)
	}

	seenSelf := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case wg := <-got:
			seenSelf[wg.PrivateKey] = true
			if len(wg.Peers) != 1 {
				t.Errorf("each member must have exactly 1 peer, got %d", len(wg.Peers))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out; seen=%v", seenSelf)
		}
	}
	if !seenSelf["priv1"] || !seenSelf["priv2"] {
		t.Errorf("both members must receive their own key: %v", seenSelf)
	}
}

func TestPublishMesh_NonNATSBus(t *testing.T) {
	a := &Adapter{} // no bus → natsConnFromBus errors
	_, err := a.PublishMesh(nil, netip.MustParsePrefix("10.9.0.0/24"), 0)
	if err == nil {
		t.Fatal("expected error without a NATS bus")
	}
}
