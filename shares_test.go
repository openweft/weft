package weft

import (
	"context"
	"strings"
	"testing"
)

func TestShareRegistry_EmptyStartsClean(t *testing.T) {
	reg, err := loadShareRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := reg.list(""); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d rows", len(got))
	}
}

func TestShareRegistry_CreateRoundTrips(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadShareRegistry(context.Background(), storage)
	s, created, err := reg.create("proj-1", "scratch", 50, false, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Error("first create should report created=true")
	}
	if s.Backend != "cubefs" {
		t.Errorf("empty backend should default to cubefs, got %q", s.Backend)
	}
	if s.Status != "provisioning" {
		t.Errorf("initial status should be provisioning, got %q", s.Status)
	}
	if s.SizeGB != 50 {
		t.Errorf("size lost: %d", s.SizeGB)
	}
	// Reload — name index must be rebuilt.
	reg2, err := loadShareRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(s.UUID)
	if !ok || got.Name != "scratch" {
		t.Fatalf("reload lost row: %+v ok=%v", got, ok)
	}
	// Idempotent create after reload.
	if _, created2, _ := reg2.create("proj-1", "scratch", 200, true, "other"); created2 {
		t.Error("name index not rebuilt — idempotency broke after reload")
	}
}

func TestShareRegistry_CreateValidation(t *testing.T) {
	reg, _ := loadShareRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name, proj, n   string
		size            int64
		wantErrContains string
	}{
		{"no project", "", "n", 1, "project_uuid"},
		{"no name", "p", "", 1, "name"},
		{"zero size", "p", "n", 0, "size_gb"},
		{"negative size", "p", "n", -5, "size_gb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := reg.create(tc.proj, tc.n, tc.size, false, ""); err == nil {
				t.Error("expected error")
			} else if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("error must mention %q, got: %v", tc.wantErrContains, err)
			}
		})
	}
}

func TestShareRegistry_CreateIdempotent(t *testing.T) {
	reg, _ := loadShareRegistry(context.Background(), NewMemStorage())
	s1, _, _ := reg.create("p", "scratch", 50, false, "cubefs")
	s2, created, err := reg.create("p", "scratch", 100, true, "other")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created {
		t.Error("repeat create with same (project,name) should report created=false")
	}
	if s2.UUID != s1.UUID || s2.SizeGB != 50 || s2.Readonly != false {
		t.Errorf("repeat create clobbered fields: %+v", s2)
	}
}

func TestShareRegistry_ResizeMonotonic(t *testing.T) {
	reg, _ := loadShareRegistry(context.Background(), NewMemStorage())
	s, _, _ := reg.create("p", "scratch", 50, false, "")
	// Grow.
	upd, err := reg.resize(s.UUID, 100)
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if upd.SizeGB != 100 {
		t.Errorf("new size not applied: %d", upd.SizeGB)
	}
	// Shrink (refused).
	if _, err := reg.resize(s.UUID, 80); err == nil {
		t.Error("shrink must refuse")
	}
	// Same size (refused — new must be strictly greater).
	if _, err := reg.resize(s.UUID, 100); err == nil {
		t.Error("no-op resize must refuse (must be strictly greater)")
	}
	// Unknown UUID.
	if _, err := reg.resize("no-such", 200); err == nil {
		t.Error("resize unknown UUID must error")
	}
}

func TestShareRegistry_DeleteCleansIndex(t *testing.T) {
	reg, _ := loadShareRegistry(context.Background(), NewMemStorage())
	s, _, _ := reg.create("p", "scratch", 50, false, "")
	if err := reg.delete(s.UUID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := reg.lookupByUUID(s.UUID); ok {
		t.Error("deleted row still surfaces via UUID lookup")
	}
	// Recreate must succeed (name index freed).
	if _, created, err := reg.create("p", "scratch", 50, false, ""); err != nil || !created {
		t.Errorf("recreate after delete failed: created=%v err=%v", created, err)
	}
	if err := reg.delete("no-such"); err == nil {
		t.Error("delete unknown UUID must error")
	}
}

func TestShareRegistry_ListFiltersByProject(t *testing.T) {
	reg, _ := loadShareRegistry(context.Background(), NewMemStorage())
	_, _, _ = reg.create("proj-A", "a", 1, false, "")
	_, _, _ = reg.create("proj-A", "b", 1, false, "")
	_, _, _ = reg.create("proj-B", "c", 1, false, "")
	if got := reg.list(""); len(got) != 3 {
		t.Errorf("list(\"\") should return 3, got %d", len(got))
	}
	gotA := reg.list("proj-A")
	if len(gotA) != 2 {
		t.Errorf("list(proj-A) should return 2, got %d", len(gotA))
	}
	// Sort by (project, name).
	if gotA[0].Name != "a" || gotA[1].Name != "b" {
		t.Errorf("list not sorted by name: %+v", gotA)
	}
}
