package weft

import (
	"context"
	"testing"
)

// TestTenantCapRegistry_RoundTrip pins the new tenant-keyed cap
// registry : set, get, persistence, clear-via-zero.
func TestTenantCapRegistry_RoundTrip(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadTenantCapRegistry(context.Background(), storage)
	if err != nil {
		t.Fatal(err)
	}
	want := TenantQuota{CPUCount: 128, MemoryGiB: 512, FloatingIPs: 24}
	if err := reg.set("tenant-1", want); err != nil {
		t.Fatal(err)
	}
	if got := reg.get("tenant-1"); got != want {
		t.Errorf("get(tenant-1) = %+v, want %+v", got, want)
	}
	// Persistence round-trip.
	reg2, err := loadTenantCapRegistry(context.Background(), storage)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg2.get("tenant-1"); got != want {
		t.Errorf("after reload get(tenant-1) = %+v, want %+v", got, want)
	}
	// Clear via zero quota deletes the entry.
	if err := reg.set("tenant-1", TenantQuota{}); err != nil {
		t.Fatal(err)
	}
	if got := reg.get("tenant-1"); got != (TenantQuota{}) {
		t.Errorf("after clear get(tenant-1) = %+v, want zero", got)
	}
	if _, present := reg.byUUID["tenant-1"]; present {
		t.Errorf("clear should evict the in-memory entry")
	}
}

// TestAdapter_TenantAllocation_AggregatesProjectQuotas covers the
// per-project sum that powers GetTenantQuota.Allocated.
func TestAdapter_TenantAllocation_AggregatesProjectQuotas(t *testing.T) {
	a := newAdapterForTest(t)
	// Two tenanted projects + one untenanted.
	p1, _, _ := a.projects.getOrCreate("alpha")
	p2, _, _ := a.projects.getOrCreate("beta")
	p3, _, _ := a.projects.getOrCreate("gamma")
	_ = a.projects.setTenant(p1.UUID, "tenant-A")
	_ = a.projects.setTenant(p2.UUID, "tenant-A")
	// p3 left untenanted.

	_ = a.SetTenantQuota(p1.UUID, TenantQuota{CPUCount: 4, MemoryGiB: 16})
	_ = a.SetTenantQuota(p2.UUID, TenantQuota{CPUCount: 8, MemoryGiB: 32, FloatingIPs: 2})
	_ = a.SetTenantQuota(p3.UUID, TenantQuota{CPUCount: 100}) // would-pollute-if-aggregated

	got := a.TenantAllocation("tenant-A")
	if got.CPUCount != 12 || got.MemoryGiB != 48 || got.FloatingIPs != 2 {
		t.Errorf("TenantAllocation(tenant-A) = %+v, want CPU=12 RAM=48 FIP=2", got)
	}

	// Unknown tenant returns zero — operators can read this shape as
	// "no projects bound to this tenant yet".
	if got := a.TenantAllocation("unknown"); got != (TenantQuota{}) {
		t.Errorf("TenantAllocation(unknown) = %+v, want zero", got)
	}
}

// Minimal adapter scaffold for the test. We deliberately don't
// boot the full New() path because the tenant_quotas/tenant_caps
// registries are what we want to exercise.
func newAdapterForTest(t *testing.T) *Adapter {
	t.Helper()
	a := &Adapter{
		stateDir:       t.TempDir(),
		storageFactory: func(_ string) Storage { return NewMemStorage() },
		bus:            NewEventBus(),
	}
	a.initProjects()
	a.initTenantQuotas()
	a.initTenantCaps()
	return a
}
