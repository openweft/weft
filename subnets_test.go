package weft

import (
	"context"
	"testing"
)

func TestSubnetRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadSubnetRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestSubnetRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadSubnetRegistry(context.Background(), storage)
	s, created, err := reg.create("net-1", "proj-1", "primary", "vlan-100", "10.0.0.0/24", "10.0.0.1", []string{"1.1.1.1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if s.UUID == "" || len(s.UUID) != 36 {
		t.Errorf("UUID should be 36-char v4, got %q", s.UUID)
	}
	if s.NetworkUUID != "net-1" || s.CIDR != "10.0.0.0/24" || len(s.DNSServers) != 1 {
		t.Errorf("fields drifted: %+v", s)
	}
	// Reload — networkIdx must be rebuilt.
	reg2, err := loadSubnetRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reg2.list("net-1"); len(got) != 1 {
		t.Errorf("network index not rebuilt on load: count=%d", len(got))
	}
}

func TestSubnetRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadSubnetRegistry(context.Background(), NewMemStorage())
	s1, _, _ := reg.create("net-1", "p", "primary", "", "10.0.0.0/24", "10.0.0.1", nil)
	s2, created, err := reg.create("net-1", "p", "different", "", "10.0.0.0/24", "10.0.0.2", []string{"8.8.8.8"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same (network, cidr) should report created=false")
	}
	if s2.UUID != s1.UUID || s2.Gateway != "10.0.0.1" {
		t.Errorf("idempotent create must return existing row unchanged: %+v", s2)
	}
}

func TestSubnetRegistry_CreateValidation(t *testing.T) {
	reg, _ := loadSubnetRegistry(context.Background(), NewMemStorage())
	if _, _, err := reg.create("", "p", "n", "", "10.0.0.0/24", "", nil); err == nil {
		t.Error("empty network_uuid must error")
	}
	if _, _, err := reg.create("net-1", "p", "n", "", "", "", nil); err == nil {
		t.Error("empty cidr must error")
	}
}

func TestSubnetRegistry_UpdatePartialPatch(t *testing.T) {
	reg, _ := loadSubnetRegistry(context.Background(), NewMemStorage())
	s, _, _ := reg.create("net-1", "p", "primary", "desc", "10.0.0.0/24", "10.0.0.1", []string{"1.1.1.1"})

	// Only update name.
	upd, err := reg.update(s.UUID, "renamed", "", "", false, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "renamed" || upd.Gateway != "10.0.0.1" || len(upd.DNSServers) != 1 {
		t.Errorf("partial patch broke: %+v", upd)
	}

	// Replace DNS servers.
	upd2, _ := reg.update(s.UUID, "", "", "", false, []string{"8.8.8.8", "8.8.4.4"})
	if len(upd2.DNSServers) != 2 || upd2.DNSServers[0] != "8.8.8.8" {
		t.Errorf("DNS replace failed: %+v", upd2)
	}

	// Clear DNS servers.
	upd3, _ := reg.update(s.UUID, "", "", "", true, nil)
	if len(upd3.DNSServers) != 0 {
		t.Errorf("clearDNS=true should zero the list, got: %+v", upd3.DNSServers)
	}

	if _, err := reg.update("no-such", "x", "", "", false, nil); err == nil {
		t.Error("update of unknown UUID must error")
	}
}

func TestSubnetRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadSubnetRegistry(context.Background(), NewMemStorage())
	s, _, _ := reg.create("net-1", "p", "primary", "", "10.0.0.0/24", "", nil)
	if err := reg.delete(s.UUID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(s.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	if got := reg.list("net-1"); len(got) != 0 {
		t.Error("network index leaked after delete")
	}
	if err := reg.delete("no-such"); err == nil {
		t.Error("delete unknown UUID must error")
	}
}

func TestSubnetRegistry_ListFiltersByNetwork(t *testing.T) {
	reg, _ := loadSubnetRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("net-A", "p", "", "", "10.0.0.0/24", "", nil)
	_, _, _ = reg.create("net-A", "p", "", "", "10.0.1.0/24", "", nil)
	_, _, _ = reg.create("net-B", "p", "", "", "10.0.2.0/24", "", nil)
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("list(\"\") should return 3, got %d", len(got))
	}
	gotA := reg.list("net-A")
	if len(gotA) != 2 {
		t.Errorf("list(net-A) should return 2, got %d", len(gotA))
	}
	// Sort by cidr inside same network.
	if gotA[0].CIDR != "10.0.0.0/24" || gotA[1].CIDR != "10.0.1.0/24" {
		t.Errorf("list not sorted by cidr: %+v", gotA)
	}
}
