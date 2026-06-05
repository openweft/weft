package weft

import (
	"context"
	"strings"
	"testing"
)

func TestRackRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadRackRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestRackRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadRackRegistry(context.Background(), storage)
	rk, created, err := reg.create("az-1", "R1", "Rack 1", "", 42, true /* azExists */)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if rk.AZUUID != "az-1" || rk.Code != "R1" || rk.HeightU != 42 || rk.Status != "active" {
		t.Errorf("row fields drifted : %+v", rk)
	}

	// Reload to confirm persistence.
	reg2, err := loadRackRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(rk.UUID)
	if !ok || got.Code != rk.Code {
		t.Errorf("reload lost the rack : %+v vs %+v", got, rk)
	}
}

func TestRackRegistry_CreateRefusesUnknownAZ(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	if _, _, err := reg.create("az-ghost", "R1", "", "", 0, false /* azExists=false */); err == nil {
		t.Error("create with unknown az_uuid must error")
	}
}

func TestRackRegistry_CreateRefusesEmptyKeys(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	if _, _, err := reg.create("", "R1", "", "", 0, true); err == nil {
		t.Error("create with empty az_uuid must error")
	}
	if _, _, err := reg.create("az-1", "", "", "", 0, true); err == nil {
		t.Error("create with empty code must error")
	}
}

func TestRackRegistry_SameCodeAcrossAZsAllowed(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	r1, c1, _ := reg.create("az-1", "R1", "", "", 0, true)
	r2, c2, _ := reg.create("az-2", "R1", "", "", 0, true)
	if !c1 || !c2 {
		t.Error("each AZ should accept its own R1")
	}
	if r1.UUID == r2.UUID {
		t.Error("racks under different AZs must mint distinct UUIDs")
	}
}

func TestRackRegistry_SameCodeSameAZIdempotent(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	r1, _, _ := reg.create("az-1", "R1", "first", "", 10, true)
	r2, created, _ := reg.create("az-1", "R1", "different", "draining", 99, true)
	if created {
		t.Error("repeat create in same AZ should report created=false")
	}
	if r2.UUID != r1.UUID {
		t.Error("repeat create should return existing row")
	}
	if r2.Name != "first" {
		t.Error("repeat create must NOT clobber the existing row")
	}
}

func TestRackRegistry_UpdatePartialPatch(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	r, _, _ := reg.create("az-1", "R1", "Rack 1", "active", 42, true)

	upd, err := reg.update(r.UUID, "Renamed", "", -1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "Renamed" || upd.Status != "active" || upd.HeightU != 42 {
		t.Errorf("partial patch broke : %+v", upd)
	}

	// heightU = 0 is a valid explicit value ; check that distinct from -1.
	upd2, _ := reg.update(r.UUID, "", "", 0)
	if upd2.HeightU != 0 {
		t.Errorf("heightU=0 must be honoured (got %d)", upd2.HeightU)
	}

	// heightU = -1 keeps current (now 0).
	upd3, _ := reg.update(r.UUID, "", "draining", -1)
	if upd3.HeightU != 0 {
		t.Errorf("heightU=-1 should keep current 0 (got %d)", upd3.HeightU)
	}
}

func TestRackRegistry_DeleteRefusesWithHosts(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	r, _, _ := reg.create("az-1", "R1", "", "", 0, true)
	if _, err := reg.delete(r.UUID, 2); err == nil {
		t.Error("delete with hosts > 0 must refuse")
	} else if !strings.Contains(err.Error(), "2 host") {
		t.Errorf("error should report blocking count, got: %v", err)
	}
}

func TestRackRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	r, _, _ := reg.create("az-1", "R1", "", "", 0, true)
	if _, err := reg.delete(r.UUID, 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(r.UUID); ok {
		t.Error("deleted row still in UUID index")
	}
	// Re-creating under the same (az, code) should now succeed as a new row.
	r2, created, _ := reg.create("az-1", "R1", "", "", 0, true)
	if !created || r2.UUID == r.UUID {
		t.Error("post-delete create should mint a fresh row")
	}
}

func TestRackRegistry_ListFiltersByAZ(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	reg.create("az-1", "R1", "", "", 0, true)
	reg.create("az-1", "R2", "", "", 0, true)
	reg.create("az-2", "R1", "", "", 0, true)
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("unfiltered list : got %d, want 3", len(got))
	}
	if got := reg.list("az-1"); len(got) != 2 {
		t.Errorf("az-1 filtered : got %d, want 2", len(got))
	}
	if got := reg.list("az-ghost"); len(got) != 0 {
		t.Errorf("az-ghost filtered : got %d, want 0", len(got))
	}
}

func TestRackRegistry_CountForAZ(t *testing.T) {
	reg, _ := loadRackRegistry(context.Background(), NewMemStorage())
	reg.create("az-1", "R1", "", "", 0, true)
	reg.create("az-1", "R2", "", "", 0, true)
	reg.create("az-2", "R1", "", "", 0, true)
	if got := reg.countForAZ("az-1"); got != 2 {
		t.Errorf("az-1 count : got %d, want 2", got)
	}
	if got := reg.countForAZ("az-ghost"); got != 0 {
		t.Errorf("ghost count : got %d, want 0", got)
	}
}
