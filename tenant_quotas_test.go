//go:build darwin && cgo

package weft

// tenant_quotas_test.go pins the per-project hard-cap logic the
// CreateVM / RegisterMicroVM / CreateVolume handlers consult.
// The tests exercise the three dimensions (cpu_count,
// memory_gib, volume_gib) independently and assert on the
// ResourceExhausted return so the wire-level contract is stable.

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// quotaFixture spins up an Adapter rooted at a temp dir + creates
// one project. Each test stamps its own caps via SetTenantQuota
// against that project ; the helpers Then() use the project UUID.
type quotaFixture struct {
	a        *Adapter
	projUUID string
}

func newQuotaFixture(t *testing.T) quotaFixture {
	t.Helper()
	dir := t.TempDir()
	a := New(dir).(*Adapter)
	p, _, err := a.CreateProject("alpha")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return quotaFixture{a: a, projUUID: p.UUID}
}

func TestTenantQuota_RoundTrip(t *testing.T) {
	f := newQuotaFixture(t)
	want := TenantQuota{CPUCount: 16, MemoryGiB: 64, VolumeGiB: 500}
	if err := f.a.SetTenantQuota(f.projUUID, want); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	got := f.a.TenantQuota(f.projUUID)
	if got != want {
		t.Errorf("TenantQuota: got %+v, want %+v", got, want)
	}
}

func TestTenantQuota_PersistsAcrossLoad(t *testing.T) {
	dir := t.TempDir()
	a1 := New(dir).(*Adapter)
	p, _, err := a1.CreateProject("alpha")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	want := TenantQuota{CPUCount: 4, MemoryGiB: 8, VolumeGiB: 100}
	if err := a1.SetTenantQuota(p.UUID, want); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	// Fresh Adapter against the same state dir : the registry
	// reload via storageFactory must see the persisted block.
	a2 := New(dir).(*Adapter)
	got := a2.TenantQuota(p.UUID)
	if got != want {
		t.Errorf("after reload: got %+v, want %+v", got, want)
	}
}

func TestTenantQuota_ZeroQuotaClearsEntry(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{CPUCount: 4}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := f.a.TenantQuota(f.projUUID); got.CPUCount != 4 {
		t.Fatalf("pre-clear: got %+v, want CPUCount=4", got)
	}
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := f.a.TenantQuota(f.projUUID); got != (TenantQuota{}) {
		t.Errorf("after clear: got %+v, want zero", got)
	}
}

func TestEnforceVM_UnlimitedAllows(t *testing.T) {
	f := newQuotaFixture(t)
	// No quota set ⇒ unlimited ⇒ enforce returns nil even for
	// large requests.
	if err := f.a.EnforceTenantQuotaForVM(f.projUUID, 999, 999*1024); err != nil {
		t.Fatalf("unlimited should allow: %v", err)
	}
}

func TestEnforceVM_CPULimitTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{CPUCount: 4}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// First 2 cpu fits.
	if err := f.a.EnforceTenantQuotaForVM(f.projUUID, 2, 0); err != nil {
		t.Fatalf("2 cpu under 4 cap: %v", err)
	}
	// Register the 2-cpu VM so allocation reflects 2 already used.
	// Bypass Adapter.RegisterVM (which requires a driver handle) —
	// projectAllocation only reads the vmRegistry, so a direct
	// create is fine for the quota arithmetic the test exercises.
	host, err := f.a.RegisterHost(RegisterHostSpec{Hostname: "h1", Endpoint: "tcp://h1:1"})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	if _, err := f.a.vmReg.create(CreateVMSpec{
		ProjectUUID: f.projUUID,
		Name:        "vm1",
		HostUUID:    host.UUID,
		CPUCount:    2,
	}); err != nil {
		t.Fatalf("vmReg.create: %v", err)
	}
	// Now asking for 3 more would breach (2+3 > 4).
	err = f.a.EnforceTenantQuotaForVM(f.projUUID, 3, 0)
	if err == nil {
		t.Fatal("expected ResourceExhausted, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("got code %s, want ResourceExhausted", status.Code(err))
	}
	// Re-checking with no new resources (RegisterMicroVM-style)
	// stays clean : 2 <= 4.
	if err := f.a.EnforceTenantQuotaForVM(f.projUUID, 0, 0); err != nil {
		t.Errorf("0,0 should fit existing allocation: %v", err)
	}
}

func TestEnforceVM_MemoryCeilDiv(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{MemoryGiB: 2}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// 2049 MiB = 3 GiB after ceil-div ; > 2 GiB cap, must trip.
	err := f.a.EnforceTenantQuotaForVM(f.projUUID, 0, 2049)
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("2049 MiB ceil-div should trip 2 GiB cap, got %v", err)
	}
	// 2048 MiB = 2 GiB after ceil-div, fits exactly.
	if err := f.a.EnforceTenantQuotaForVM(f.projUUID, 0, 2048); err != nil {
		t.Errorf("2048 MiB == 2 GiB cap should fit: %v", err)
	}
}

func TestEnforceVolume_LimitTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{VolumeGiB: 50}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// 40 GiB under 50, OK.
	if err := f.a.EnforceTenantQuotaForVolume(f.projUUID, 40); err != nil {
		t.Fatalf("40 GiB under 50: %v", err)
	}
	// Create the 40 GiB volume.
	if _, err := f.a.CreateVolume(CreateVolumeSpec{
		ProjectUUID: f.projUUID,
		Name:        "v1",
		SizeGiB:     40,
		Format:      VolumeFormatRaw,
	}); err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	// 20 GiB more would breach (40+20 > 50).
	err := f.a.EnforceTenantQuotaForVolume(f.projUUID, 20)
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got %v", err)
	}
	// 10 GiB fits exactly.
	if err := f.a.EnforceTenantQuotaForVolume(f.projUUID, 10); err != nil {
		t.Errorf("10 GiB should fit (40+10=50): %v", err)
	}
}

func TestEnforceVolume_Unlimited(t *testing.T) {
	f := newQuotaFixture(t)
	// Zero cap = unlimited ; enforce should never trip.
	if err := f.a.EnforceTenantQuotaForVolume(f.projUUID, 100000); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
}

// TestEnforceVM_RejectsWithGRPCCode is a smoke test that the
// returned error carries codes.ResourceExhausted verbatim so
// gRPC clients can match on it without string parsing — that's
// the wire contract docs/operations/tenant-quotas.md commits to.
func TestEnforceVM_RejectsWithGRPCCode(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{CPUCount: 1}); err != nil {
		t.Fatalf("set: %v", err)
	}
	err := f.a.EnforceTenantQuotaForVM(f.projUUID, 5, 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("code = %s, want ResourceExhausted", got)
	}
	// The error message should mention the dimension that tripped.
	if want := "cpu"; !contains(err.Error(), want) {
		t.Errorf("error %q should mention %q", err.Error(), want)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEnforceVM_ContextlessIsPureFunction sanity-checks that
// EnforceTenantQuotaForVM is callable without a Caller in ctx —
// the handler runs it after AuthorizeProject so the project UUID
// is the only thing it needs, no Caller dereference.
func TestEnforceVM_ContextlessIsPureFunction(t *testing.T) {
	_ = context.Background()
	f := newQuotaFixture(t)
	if err := f.a.EnforceTenantQuotaForVM(f.projUUID, 1, 1024); err != nil {
		t.Errorf("no caller, no cap, should allow: %v", err)
	}
}
