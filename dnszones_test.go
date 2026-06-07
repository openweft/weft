package weft

import (
	"context"
	"strings"
	"testing"
)

func TestDNSZoneRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestDNSZoneRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadDNSZoneRegistry(context.Background(), storage)
	z, created, err := reg.create("proj-1", "acme.example.com", "hostmaster@acme.example.com", 3600)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if z.UUID == "" || z.TTL != 3600 {
		t.Errorf("fields drifted: %+v", z)
	}
	// Reload — name index must be rebuilt.
	reg2, err := loadDNSZoneRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByName("acme.example.com")
	if !ok || got.UUID != z.UUID {
		t.Errorf("name index not rebuilt on load: %+v", got)
	}
}

func TestDNSZoneRegistry_CreateDefaultsTTL(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	z, _, _ := reg.create("p", "z.example.com", "", 0)
	if z.TTL != 3600 {
		t.Errorf("ttl=0 should default to 3600, got %d", z.TTL)
	}
}

func TestDNSZoneRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	z1, _, _ := reg.create("p", "acme.example.com", "old@x", 100)
	z2, created, err := reg.create("p", "acme.example.com", "new@x", 200)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same name should report created=false")
	}
	if z2.UUID != z1.UUID || z2.SOAEmail != "old@x" || z2.TTL != 100 {
		t.Errorf("repeat create clobbered fields: %+v", z2)
	}
}

func TestDNSZoneRegistry_CreateRejectsEmptyName(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	if _, _, err := reg.create("p", "", "", 0); err == nil {
		t.Error("create with empty name must error")
	}
}

func TestDNSZoneRegistry_UpdatePartialPatch(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	z, _, _ := reg.create("p", "z.example.com", "old@x", 100)

	upd, err := reg.update(z.UUID, "new@x", -1)
	if err != nil {
		t.Fatalf("update soa only: %v", err)
	}
	if upd.SOAEmail != "new@x" || upd.TTL != 100 {
		t.Errorf("soa-only patch leaked: %+v", upd)
	}
	upd2, _ := reg.update(z.UUID, "", 200)
	if upd2.TTL != 200 || upd2.SOAEmail != "new@x" {
		t.Errorf("ttl-only patch leaked: %+v", upd2)
	}
	if _, err := reg.update("no-such", "x", -1); err == nil {
		t.Error("update on unknown UUID must error")
	}
}

func TestDNSZoneRegistry_DeleteRefusesCascade(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	z, _, _ := reg.create("p", "z.example.com", "", 0)
	if _, err := reg.delete(z.UUID, 3); err == nil {
		t.Error("delete with records > 0 must refuse")
	} else if !strings.Contains(err.Error(), "3 record") {
		t.Errorf("error should report blocking count, got: %v", err)
	}
}

func TestDNSZoneRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	z, _, _ := reg.create("p", "z.example.com", "", 0)
	if _, err := reg.delete(z.UUID, 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(z.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	if _, ok := reg.lookupByName("z.example.com"); ok {
		t.Error("deleted row still surfaces via name lookup")
	}
	if _, err := reg.delete("no-such", 0); err == nil {
		t.Error("delete of unknown UUID must error")
	}
}

func TestDNSZoneRegistry_ListFiltersByProject(t *testing.T) {
	reg, _ := loadDNSZoneRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("proj-A", "a.example.com", "", 0)
	_, _, _ = reg.create("proj-A", "b.example.com", "", 0)
	_, _, _ = reg.create("proj-B", "c.example.com", "", 0)
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("list(\"\") should return 3, got %d", len(got))
	}
	gotA := reg.list("proj-A")
	if len(gotA) != 2 {
		t.Errorf("list(proj-A) should return 2, got %d", len(gotA))
	}
	// Sort by name.
	if gotA[0].Name != "a.example.com" || gotA[1].Name != "b.example.com" {
		t.Errorf("list not sorted by name: %+v", gotA)
	}
}
