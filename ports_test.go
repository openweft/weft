package weft

import (
	"context"
	"strings"
	"testing"
)

func TestPortRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadPortRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadPortRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
}

func TestPortRegistry_CreateAndLookup(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	p, err := reg.create(CreatePortSpec{
		ProjectUUID: "p",
		VMUUID:      "vm-1",
		NetworkUUID: "net-1",
		MAC:         "52:54:00:00:00:01",
		IP:          "10.0.0.10",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.UUID == "" {
		t.Errorf("port should have UUID")
	}
	got, ok := reg.lookupByUUID(p.UUID)
	if !ok || got.UUID != p.UUID {
		t.Errorf("lookupByUUID failed")
	}
}

func TestPortRegistry_RejectsMissingFields(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	cases := []CreatePortSpec{
		{VMUUID: "vm", NetworkUUID: "n", MAC: "m", IP: "10.0.0.1"},                  // empty project
		{ProjectUUID: "p", NetworkUUID: "n", MAC: "m", IP: "10.0.0.1"},              // empty vm
		{ProjectUUID: "p", VMUUID: "vm", MAC: "m", IP: "10.0.0.1"},                  // empty network
		{ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "n", IP: "10.0.0.1"},          // empty mac
		{ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "n", MAC: "m"},                // empty ip
	}
	for i, spec := range cases {
		if _, err := reg.create(spec); err == nil {
			t.Errorf("case %d: empty required field should be rejected", i)
		}
	}
}

func TestPortRegistry_IPUniquePerNetwork(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	if _, err := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: "net-1",
		MAC: "52:54:00:00:00:01", IP: "10.0.0.10",
	}); err != nil {
		t.Fatal(err)
	}
	// Same IP on the same network: rejected.
	_, err := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-2", NetworkUUID: "net-1",
		MAC: "52:54:00:00:00:02", IP: "10.0.0.10",
	})
	if err == nil {
		t.Errorf("duplicate IP on same network should be rejected")
	}
	// Same IP on a DIFFERENT network: allowed (each network has
	// its own address space).
	_, err = reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-3", NetworkUUID: "net-2",
		MAC: "52:54:00:00:00:03", IP: "10.0.0.10",
	})
	if err != nil {
		t.Errorf("same IP on different network should be allowed: %v", err)
	}
}

func TestPortRegistry_MACUniquePerNetwork(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: "net-1",
		MAC: "aa:bb:cc:dd:ee:ff", IP: "10.0.0.1",
	})
	_, err := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-2", NetworkUUID: "net-1",
		MAC: "aa:bb:cc:dd:ee:ff", IP: "10.0.0.2",
	})
	if err == nil {
		t.Errorf("duplicate MAC on same network should be rejected")
	}
}

func TestPortRegistry_ListForVMAndNetwork(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	// vm-1 has two NICs (on net-1 and net-2).
	_, _ = reg.create(CreatePortSpec{ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: "net-1", MAC: "01:00:00:00:00:01", IP: "10.1.0.1"})
	_, _ = reg.create(CreatePortSpec{ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: "net-2", MAC: "02:00:00:00:00:01", IP: "10.2.0.1"})
	// vm-2 is on net-1 only.
	_, _ = reg.create(CreatePortSpec{ProjectUUID: "p", VMUUID: "vm-2", NetworkUUID: "net-1", MAC: "01:00:00:00:00:02", IP: "10.1.0.2"})

	if got := reg.listForVM("vm-1"); len(got) != 2 {
		t.Errorf("listForVM(vm-1) = %d, want 2", len(got))
	}
	if got := reg.listForVM("vm-2"); len(got) != 1 {
		t.Errorf("listForVM(vm-2) = %d, want 1", len(got))
	}
	if got := reg.listForNetwork("net-1"); len(got) != 2 {
		t.Errorf("listForNetwork(net-1) = %d, want 2", len(got))
	}
	// Sorted by IP: 10.1.0.1 then 10.1.0.2.
	got := reg.listForNetwork("net-1")
	if got[0].IP > got[1].IP {
		t.Errorf("listForNetwork not sorted by IP: %v", []string{got[0].IP, got[1].IP})
	}
	if got := reg.listForVM("nope"); len(got) != 0 {
		t.Errorf("unknown VM should return empty")
	}
}

func TestPortRegistry_Delete(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	p, _ := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "net",
		MAC: "01:02:03:04:05:06", IP: "10.0.0.1",
	})
	if err := reg.delete(p.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookupByUUID(p.UUID); ok {
		t.Errorf("port should be gone after delete")
	}
	// Indexes clean.
	if _, ok := reg.ipIdx[portIPKey("net", "10.0.0.1")]; ok {
		t.Errorf("IP index should be cleaned up")
	}
	if _, ok := reg.macIdx[portMACKey("net", "01:02:03:04:05:06")]; ok {
		t.Errorf("MAC index should be cleaned up")
	}
	if _, ok := reg.vmIdx["vm"]; ok {
		t.Errorf("VM index should be cleaned up")
	}
	if _, ok := reg.networkIdx["net"]; ok {
		t.Errorf("network index should be cleaned up")
	}
	// Recreate with same IP+MAC now works.
	if _, err := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "net",
		MAC: "01:02:03:04:05:06", IP: "10.0.0.1",
	}); err != nil {
		t.Errorf("recreate after delete should work: %v", err)
	}
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown UUID should be rejected")
	}
}

func TestPortRegistry_SetWireguardPubKey(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	p, _ := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "net",
		MAC: "01:02:03:04:05:06", IP: "10.0.0.1",
		WireguardPubKey: "key-v1",
	})
	if err := reg.setWireguardPubKey(p.UUID, "key-v2"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.lookupByUUID(p.UUID)
	if got.WireguardPubKey != "key-v2" {
		t.Errorf("pubkey not rotated: %q", got.WireguardPubKey)
	}
	if err := reg.setWireguardPubKey("nope", "x"); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestPortRegistry_SetSecurityGroups(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	p, _ := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: "net",
		MAC: "01:02:03:04:05:06", IP: "10.0.0.1",
	})
	if err := reg.setSecurityGroups(p.UUID, []string{"sg-1", "sg-2"}); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.lookupByUUID(p.UUID)
	if len(got.SecurityGroups) != 2 {
		t.Errorf("SG count = %d, want 2", len(got.SecurityGroups))
	}
	// Clear.
	_ = reg.setSecurityGroups(p.UUID, nil)
	got, _ = reg.lookupByUUID(p.UUID)
	if len(got.SecurityGroups) != 0 {
		t.Errorf("SGs should be cleared: %v", got.SecurityGroups)
	}
}

func TestPortRegistry_PortsReferencingSG(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	pA, _ := reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: "net",
		MAC: "01:00:00:00:00:01", IP: "10.0.0.1",
		SecurityGroups: []string{"sg-web", "sg-shared"},
	})
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-2", NetworkUUID: "net",
		MAC: "01:00:00:00:00:02", IP: "10.0.0.2",
		SecurityGroups: []string{"sg-shared"},
	})
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-3", NetworkUUID: "net",
		MAC: "01:00:00:00:00:03", IP: "10.0.0.3",
		// no SG ref
	})

	if got := reg.portsReferencingSecurityGroup("sg-shared"); len(got) != 2 {
		t.Errorf("sg-shared refs = %d, want 2", len(got))
	}
	if got := reg.portsReferencingSecurityGroup("sg-web"); len(got) != 1 || got[0] != pA.UUID {
		t.Errorf("sg-web refs wrong: %v", got)
	}
	if got := reg.portsReferencingSecurityGroup("sg-unknown"); len(got) != 0 {
		t.Errorf("unknown SG should have zero refs")
	}
}

// TestPortRegistry_RoundTripViaStorage confirms HCL encode/decode
// + every secondary index rebuilds correctly.
func TestPortRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadPortRegistry(context.Background(), storage)
	p1, _ := reg.create(CreatePortSpec{
		ProjectUUID:     "p-1",
		VMUUID:          "vm-1",
		NetworkUUID:     "mesh-1",
		MAC:             "52:54:00:00:00:01",
		IP:              "10.100.0.5",
		WireguardPubKey: "abcdef1234567890",
		MeshEndpoint:    "vm1.example.com:51820",
		SecurityGroups:  []string{"sg-web"},
	})
	_, _ = reg.create(CreatePortSpec{
		ProjectUUID: "p-2",
		VMUUID:      "vm-2",
		NetworkUUID: "nat-1",
		MAC:         "52:54:00:00:00:02",
		IP:          "192.168.1.10",
	})

	blob, _ := storage.Load(context.Background())
	for _, want := range []string{
		"port \"" + p1.UUID + "\"",
		"wireguard_pub_key",
		"abcdef1234567890",
		"mesh_endpoint",
		"vm1.example.com:51820",
		"security_groups",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("HCL missing %q: %s", want, blob)
		}
	}

	reg2, err := loadPortRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	got, ok := reg2.lookupByUUID(p1.UUID)
	if !ok || got.WireguardPubKey != "abcdef1234567890" || got.MeshEndpoint != "vm1.example.com:51820" {
		t.Errorf("p1 re-load wrong: %+v", got)
	}
	if len(got.SecurityGroups) != 1 || got.SecurityGroups[0] != "sg-web" {
		t.Errorf("SGs not preserved: %v", got.SecurityGroups)
	}
	// Secondary indexes rebuilt.
	if g := reg2.listForVM("vm-1"); len(g) != 1 || g[0].UUID != p1.UUID {
		t.Errorf("vm index didn't survive reload")
	}
	if g := reg2.listForNetwork("mesh-1"); len(g) != 1 {
		t.Errorf("network index didn't survive reload")
	}
}
