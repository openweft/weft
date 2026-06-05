package weft

import (
	"context"
	"strings"
	"testing"
)

func TestAZRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadAZRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestAZRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadAZRegistry(context.Background(), storage)
	a, created, err := reg.create("DC-A", "Datacenter Alpha", "eu-west-1", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if a.Code != "DC-A" || a.Name != "Datacenter Alpha" || a.Region != "eu-west-1" || a.Status != "active" {
		t.Errorf("row fields drifted : %+v", a)
	}
	if a.UUID == "" || len(a.UUID) != 36 {
		t.Errorf("UUID should be a 36-char RFC4122 v4, got %q", a.UUID)
	}

	// Reload from storage to confirm persistence.
	reg2, err := loadAZRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(a.UUID)
	if !ok {
		t.Fatal("reloaded registry forgot the AZ we just created")
	}
	if got.Code != a.Code {
		t.Errorf("reload mismatch : %+v vs %+v", got, a)
	}
	gotByCode, ok := reg2.lookupByCode("DC-A")
	if !ok || gotByCode.UUID != a.UUID {
		t.Errorf("code → uuid index not rebuilt on load")
	}
}

func TestAZRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	a1, _, _ := reg.create("DC-A", "first", "eu-west-1", "active")
	a2, created, err := reg.create("DC-A", "different", "eu-east-1", "draining")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same code should report created=false")
	}
	if a2.UUID != a1.UUID {
		t.Error("repeat create should return the existing row, not mint a new UUID")
	}
	if a2.Name != "first" {
		t.Error("repeat create must NOT clobber the existing row's fields")
	}
}

func TestAZRegistry_CreateRejectsEmptyCode(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	if _, _, err := reg.create("", "x", "y", ""); err == nil {
		t.Error("create with empty code must error")
	}
}

func TestAZRegistry_UpdatePartialPatch(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	a, _, _ := reg.create("DC-A", "Datacenter Alpha", "eu-west-1", "active")

	// Only update name ; region + status should be preserved.
	upd, err := reg.update(a.UUID, "Renamed", "", "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "Renamed" || upd.Region != "eu-west-1" || upd.Status != "active" {
		t.Errorf("partial patch broke : %+v", upd)
	}

	// Status alone.
	upd2, _ := reg.update(a.UUID, "", "", "draining")
	if upd2.Status != "draining" || upd2.Name != "Renamed" {
		t.Errorf("status patch leaked : %+v", upd2)
	}
}

func TestAZRegistry_UpdateUnknown(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	if _, err := reg.update("no-such-uuid", "x", "y", "z"); err == nil {
		t.Error("update of unknown UUID must error")
	}
}

func TestAZRegistry_DeleteRefusesCascade(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	a, _, _ := reg.create("DC-A", "x", "eu-west-1", "")
	// Cascade still references the AZ.
	if _, _, err := reg.delete(a.UUID, 3 /* racks */, 0); err == nil {
		t.Error("delete with racks > 0 must refuse")
	} else if !strings.Contains(err.Error(), "3 rack") {
		t.Errorf("error should report the blocking count, got: %v", err)
	}
	if _, _, err := reg.delete(a.UUID, 0, 5 /* hosts */); err == nil {
		t.Error("delete with hosts > 0 must refuse")
	}
}

func TestAZRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	a, _, _ := reg.create("DC-A", "x", "eu-west-1", "")
	if _, _, err := reg.delete(a.UUID, 0, 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(a.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	if _, ok := reg.lookupByCode("DC-A"); ok {
		t.Error("deleted row still surfaces via code lookup — secondary index leaked")
	}
}

func TestAZRegistry_ListIsSortedByCode(t *testing.T) {
	reg, _ := loadAZRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("DC-B", "", "", "")
	_, _, _ = reg.create("DC-A", "", "", "")
	_, _, _ = reg.create("DC-C", "", "", "")
	got := reg.list()
	want := []string{"DC-A", "DC-B", "DC-C"}
	for i, w := range want {
		if got[i].Code != w {
			t.Errorf("list[%d].Code : got %q, want %q", i, got[i].Code, w)
		}
	}
}

func TestMintInventoryUUID_LooksRFC4122v4(t *testing.T) {
	u, err := mintInventoryUUID()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(u) != 36 {
		t.Errorf("UUID length : got %d, want 36", len(u))
	}
	// version 4 byte : the 14th char (after dashes 8,13) must be '4'
	if u[14] != '4' {
		t.Errorf("UUID version nibble : got %c, want 4 (full: %s)", u[14], u)
	}
}
