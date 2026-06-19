package weft

import (
	"context"
	"testing"
)

// TestProjectRegistry_TenantLinkage covers the v0.4.37 project →
// tenant binding : setTenant + listByTenant + HCL round-trip
// of the new TenantUUID field.
func TestProjectRegistry_TenantLinkage(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatal(err)
	}

	p1, _, err := reg.getOrCreate("alpha")
	if err != nil {
		t.Fatal(err)
	}
	p2, _, err := reg.getOrCreate("beta")
	if err != nil {
		t.Fatal(err)
	}
	p3, _, err := reg.getOrCreate("gamma")
	if err != nil {
		t.Fatal(err)
	}

	// Bind p1 + p2 to tenant T1, leave p3 untenanted.
	if err := reg.setTenant(p1.UUID, "tenant-1"); err != nil {
		t.Fatal(err)
	}
	if err := reg.setTenant(p2.UUID, "tenant-1"); err != nil {
		t.Fatal(err)
	}

	// listByTenant("tenant-1") should return p1 + p2 ; untenanted call
	// returns only p3.
	got := reg.listByTenant("tenant-1")
	if len(got) != 2 {
		t.Errorf("listByTenant(tenant-1) = %d, want 2", len(got))
	}
	untenanted := reg.listByTenant("")
	if len(untenanted) != 1 || untenanted[0].UUID != p3.UUID {
		t.Errorf("listByTenant(\"\") = %v, want [%s]", untenanted, p3.UUID)
	}

	// Persistence round-trip : reload, the bindings survive.
	reg2, err := loadProjectRegistry(context.Background(), storage)
	if err != nil {
		t.Fatal(err)
	}
	p1Reload, ok := reg2.lookupByUUID(p1.UUID)
	if !ok || p1Reload.TenantUUID != "tenant-1" {
		t.Errorf("p1 after reload TenantUUID = %q, want tenant-1", p1Reload.TenantUUID)
	}
	p3Reload, ok := reg2.lookupByUUID(p3.UUID)
	if !ok || p3Reload.TenantUUID != "" {
		t.Errorf("p3 after reload TenantUUID = %q, want empty", p3Reload.TenantUUID)
	}

	// Unbind p1 ; listByTenant(tenant-1) should drop to 1.
	if err := reg.setTenant(p1.UUID, ""); err != nil {
		t.Fatal(err)
	}
	got = reg.listByTenant("tenant-1")
	if len(got) != 1 || got[0].UUID != p2.UUID {
		t.Errorf("after unbind p1, listByTenant(tenant-1) = %v, want [%s]", got, p2.UUID)
	}

	// Unknown project UUID is an error (operator typo).
	if err := reg.setTenant("does-not-exist", "tenant-1"); err == nil {
		t.Errorf("setTenant on unknown UUID should fail")
	}
}
