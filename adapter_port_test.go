//go:build darwin

package weft

import (
	"strings"
	"testing"
)

func newAdapterForPortTest(t *testing.T) *Adapter {
	t.Helper()
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	return NewWithStorage(stateDir, factory).(*Adapter)
}

func TestAdapter_CreatePort_HappyPath_NAT(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24", Type: NetworkTypeNAT,
	})
	p, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p",
		VMUUID:      "vm-1",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		IP:          "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("create port: %v", err)
	}
	if p.UUID == "" {
		t.Errorf("created port should have UUID")
	}
}

func TestAdapter_CreatePort_HappyPath_Mesh(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "wg", CIDR: "10.100.0.0/24",
		Type: NetworkTypeMesh, MeshListenPort: 51820,
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p",
		VMUUID:      "vm-1",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		IP:          "10.100.0.5",
		WireguardPubKey: "test-pubkey",
		MeshEndpoint:    "vm1.example.com:51820",
	})
	if err != nil {
		t.Fatalf("create mesh port: %v", err)
	}
}

func TestAdapter_CreatePort_RejectsCrossProjectNetwork(t *testing.T) {
	a := newAdapterForPortTest(t)
	// Network in p-1, port claims p-2.
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p-1", Name: "main", CIDR: "10.0.0.0/24",
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p-2", VMUUID: "vm",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		IP:          "10.0.0.5",
	})
	if err == nil {
		t.Fatal("cross-project network reference should be rejected")
	}
}

func TestAdapter_CreatePort_RejectsIPOutsideCIDR(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24",
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		IP:          "192.168.1.5", // outside 10.0.0.0/24
	})
	if err == nil {
		t.Fatal("IP outside CIDR should be rejected")
	}
	if !strings.Contains(err.Error(), "cidr") {
		t.Errorf("error should mention cidr, got: %v", err)
	}
}

func TestAdapter_CreatePort_RejectsMeshFieldsOnNonMesh(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "nat", CIDR: "10.0.0.0/24", Type: NetworkTypeNAT,
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm",
		NetworkUUID:     n.UUID,
		MAC:             "52:54:00:00:00:01",
		IP:              "10.0.0.5",
		WireguardPubKey: "should-be-rejected",
	})
	if err == nil {
		t.Fatal("pubkey on non-mesh network should be rejected")
	}
}

func TestAdapter_CreatePort_RequiresPubKeyOnMesh(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "wg", CIDR: "10.100.0.0/24", Type: NetworkTypeMesh,
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm",
		NetworkUUID: n.UUID,
		MAC:         "52:54:00:00:00:01",
		IP:          "10.100.0.5",
		// no pubkey
	})
	if err == nil {
		t.Fatal("missing pubkey on mesh network should be rejected")
	}
}

func TestAdapter_CreatePort_RejectsCrossProjectSG(t *testing.T) {
	a := newAdapterForPortTest(t)
	// SG in p-1, network in p-2.
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p-1", Name: "web"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p-2", Name: "main", CIDR: "10.0.0.0/24",
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID:    "p-2", VMUUID: "vm",
		NetworkUUID:    n.UUID,
		MAC:            "52:54:00:00:00:01",
		IP:             "10.0.0.5",
		SecurityGroups: []string{sg.UUID},
	})
	if err == nil {
		t.Fatal("cross-project SG should be rejected")
	}
}

func TestAdapter_DeleteSecurityGroup_RefusedWhenPortReferences(t *testing.T) {
	a := newAdapterForPortTest(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "web"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24"})
	p, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: n.UUID,
		MAC: "52:54:00:00:00:01", IP: "10.0.0.5",
		SecurityGroups: []string{sg.UUID},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delete must refuse — port still references SG.
	err = a.DeleteSecurityGroup(sg.UUID)
	if err == nil {
		t.Fatal("delete should be refused while referenced by port")
	}
	if !strings.Contains(err.Error(), p.UUID) {
		t.Errorf("error should name the referencing port %s, got: %v", p.UUID, err)
	}

	// Clear the port's SG ref; delete now succeeds.
	if err := a.SetPortSecurityGroups(p.UUID, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSecurityGroup(sg.UUID); err != nil {
		t.Fatalf("delete after clearing ref: %v", err)
	}
}

func TestAdapter_SetPortWireguardPubKey_RejectsOnNonMesh(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "nat", CIDR: "10.0.0.0/24"})
	p, _ := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: n.UUID,
		MAC: "52:54:00:00:00:01", IP: "10.0.0.5",
	})
	if err := a.SetPortWireguardPubKey(p.UUID, "key"); err == nil {
		t.Fatal("rotate pubkey on non-mesh port should be rejected")
	}
}

func TestAdapter_SetPortWireguardPubKey_HappyPath(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "wg", CIDR: "10.100.0.0/24", Type: NetworkTypeMesh,
	})
	p, _ := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: n.UUID,
		MAC: "52:54:00:00:00:01", IP: "10.100.0.5",
		WireguardPubKey: "key-v1",
	})
	if err := a.SetPortWireguardPubKey(p.UUID, "key-v2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	got, _ := a.PortByUUID(p.UUID)
	if got.WireguardPubKey != "key-v2" {
		t.Errorf("pubkey not rotated: %q", got.WireguardPubKey)
	}
}

func TestAdapter_DeletePort_EmitsNetworkPeersChanged(t *testing.T) {
	a := newAdapterForPortTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "wg", CIDR: "10.100.0.0/24", Type: NetworkTypeMesh,
	})
	p, _ := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: n.UUID,
		MAC: "52:54:00:00:00:01", IP: "10.100.0.5",
		WireguardPubKey: "key",
	})
	// Subscribe before the delete so we see the event. SeeAll
	// bypasses the per-project ACL check on the filter — zero-
	// value EventFilter would reject project-scoped events.
	sub, unsubscribe := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer unsubscribe()

	if err := a.DeletePort(p.UUID); err != nil {
		t.Fatal(err)
	}
	// Drain a handful of events looking for network.peers_changed.
	sawPeersChanged := false
	for i := 0; i < 10; i++ {
		select {
		case ev := <-sub:
			if ev.Kind == "network.peers_changed" && ev.Subject == n.UUID {
				sawPeersChanged = true
			}
		default:
			i = 10 // exit
		}
	}
	if !sawPeersChanged {
		t.Errorf("expected network.peers_changed event after port delete")
	}
}
