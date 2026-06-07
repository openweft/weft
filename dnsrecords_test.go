package weft

import (
	"context"
	"strings"
	"testing"
)

func TestDNSRecordRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadDNSRecordRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
	if c := reg.countForZone("any"); c != 0 {
		t.Errorf("countForZone on empty should be 0, got %d", c)
	}
}

func TestDNSRecordRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadDNSRecordRegistry(context.Background(), storage)
	rec, created, err := reg.create("z1", "www", "A", "10.0.0.1", 300, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if rec.UUID == "" {
		t.Error("UUID must be minted")
	}
	// Reload — zone index must be rebuilt.
	reg2, err := loadDNSRecordRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c := reg2.countForZone("z1"); c != 1 {
		t.Errorf("zone index not rebuilt on load: count=%d", c)
	}
}

func TestDNSRecordRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadDNSRecordRegistry(context.Background(), NewMemStorage())
	r1, _, _ := reg.create("z1", "www", "A", "10.0.0.1", 300, 0)
	r2, created, err := reg.create("z1", "www", "A", "10.0.0.1", 600, 0)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same (zone,name,type,value) should report created=false")
	}
	if r2.UUID != r1.UUID || r2.TTL != 300 {
		t.Errorf("idempotent create should return existing row unchanged: %+v", r2)
	}
}

func TestDNSRecordRegistry_CreateValidation(t *testing.T) {
	reg, _ := loadDNSRecordRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name                       string
		zone, n, recordType, value string
		wantErrContains            string
	}{
		{"no zone", "", "www", "A", "1.2.3.4", "zone_uuid"},
		{"no name", "z", "", "A", "1.2.3.4", "name"},
		{"bad type", "z", "n", "AAA", "1.2.3.4", "must be one of"},
		{"no value", "z", "n", "A", "", "value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reg.create(tc.zone, tc.n, tc.recordType, tc.value, 0, 0); err == nil {
				t.Error("expected error")
			} else if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error must mention %q, got: %v", tc.wantErrContains, err)
			}
		})
	}
}

func TestDNSRecordRegistry_UpdatePartialPatch(t *testing.T) {
	reg, _ := loadDNSRecordRegistry(context.Background(), NewMemStorage())
	rec, _, _ := reg.create("z1", "mx", "MX", "mail.example.com", 600, 10)

	// Patch only the value.
	upd, err := reg.update(rec.UUID, "mail2.example.com", -1, -1)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Value != "mail2.example.com" || upd.TTL != 600 || upd.Priority != 10 {
		t.Errorf("value-only patch leaked: %+v", upd)
	}
	// Patch only TTL.
	upd2, _ := reg.update(rec.UUID, "", 1200, -1)
	if upd2.TTL != 1200 || upd2.Priority != 10 {
		t.Errorf("ttl-only patch leaked: %+v", upd2)
	}
	// Patch only Priority.
	upd3, _ := reg.update(rec.UUID, "", -1, 5)
	if upd3.Priority != 5 || upd3.TTL != 1200 {
		t.Errorf("priority-only patch leaked: %+v", upd3)
	}
	if _, err := reg.update("no-such", "x", -1, -1); err == nil {
		t.Error("update unknown UUID must error")
	}
}

func TestDNSRecordRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadDNSRecordRegistry(context.Background(), NewMemStorage())
	rec, _, _ := reg.create("z1", "www", "A", "10.0.0.1", 0, 0)
	if reg.countForZone("z1") != 1 {
		t.Error("count must be 1 before delete")
	}
	if err := reg.delete(rec.UUID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(rec.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	if reg.countForZone("z1") != 0 {
		t.Error("zone index leaked after delete")
	}
	if err := reg.delete("no-such"); err == nil {
		t.Error("delete unknown UUID must error")
	}
}

func TestDNSRecordRegistry_ListFiltersByZone(t *testing.T) {
	reg, _ := loadDNSRecordRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("z1", "a", "A", "1.1.1.1", 0, 0)
	_, _, _ = reg.create("z1", "b", "A", "2.2.2.2", 0, 0)
	_, _, _ = reg.create("z2", "c", "A", "3.3.3.3", 0, 0)
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("list(\"\") should return 3, got %d", len(got))
	}
	got1 := reg.list("z1")
	if len(got1) != 2 {
		t.Errorf("list(z1) should return 2, got %d", len(got1))
	}
	// Sort by name then type.
	if got1[0].Name != "a" || got1[1].Name != "b" {
		t.Errorf("list not sorted by name: %+v", got1)
	}
}
