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

// seedGPUVM is the quota-test helper that drops one VM with the
// given RequestedGPUs into the registry under f.projUUID, bypassing
// the driver-handle plumbing that RegisterVM exercises (the
// projectAllocation aggregate only reads vmRegistry, so a direct
// create is fine for the arithmetic the tests pin). Returns nothing
// — failures fatal the test on the spot.
func seedGPUVM(t *testing.T, f quotaFixture, hostUUID, name string, gpus []GPURequest) {
	t.Helper()
	if _, err := f.a.vmReg.create(CreateVMSpec{
		ProjectUUID:   f.projUUID,
		Name:          name,
		HostUUID:      hostUUID,
		RequestedGPUs: gpus,
	}); err != nil {
		t.Fatalf("seed vm %q: %v", name, err)
	}
}

// TestEnforceGPU_AggregateCountTrips pins the aggregate-enforcement
// gap commit 3f18e2a2d left open : three 1-GPU VMs already in the
// project + a fourth 1-GPU VM admission against a gpu_count=3 cap
// must trip. The pre-aggregate per-request-only check missed this
// because every individual request (1 ≤ 3) cleared the cap on its
// own. See the new projectAllocation GPU sum.
func TestEnforceGPU_AggregateCountTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{GPUCount: 3}); err != nil {
		t.Fatalf("set: %v", err)
	}
	host, err := f.a.RegisterHost(RegisterHostSpec{Hostname: "h-gpu", Endpoint: "tcp://h-gpu:1"})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	// Three 1-GPU VMs already in the project — total = 3, exactly
	// at cap.
	one := []GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 1}}
	seedGPUVM(t, f, host.UUID, "vm1", one)
	seedGPUVM(t, f, host.UUID, "vm2", one)
	seedGPUVM(t, f, host.UUID, "vm3", one)
	// Re-checking with nil delta stays clean (3 ≤ 3).
	if err := f.a.EnforceTenantQuotaForGPU(f.projUUID, nil); err != nil {
		t.Errorf("at-cap re-check should fit: %v", err)
	}
	// Fourth 1-GPU admission must trip : 3 + 1 > 3.
	err = f.a.EnforceTenantQuotaForGPU(f.projUUID, one)
	if err == nil {
		t.Fatal("expected ResourceExhausted on aggregate gpu_count, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("got code %s, want ResourceExhausted", status.Code(err))
	}
	if !contains(err.Error(), "gpu_count") {
		t.Errorf("error %q should mention gpu_count", err.Error())
	}
}

// TestEnforceGPU_PerRequestStillTrips pins the per-request path
// stays in force after the refactor : a single VM asking for 8
// GPUs against a gpu_count=4 cap on an empty project must still
// fail (the pre-aggregate behaviour we don't want to regress).
func TestEnforceGPU_PerRequestStillTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{GPUCount: 4}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// No VMs registered → alloc=0 ; the request alone (8) breaches.
	err := f.a.EnforceTenantQuotaForGPU(f.projUUID, []GPURequest{
		{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 8},
	})
	if err == nil {
		t.Fatal("expected ResourceExhausted on per-request gpu_count, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("got code %s, want ResourceExhausted", status.Code(err))
	}
}

// TestEnforceGPU_AggregateMemoryTrips pins the gpu_memory_gib
// dimension : three H200 VMs (3 × 141 = 423 GiB already
// allocated) + a fourth H200 admission against a 400 GiB cap
// must trip. Exercises the static gpuModelMemoryGiB lookup
// + projectAllocation memory sum.
func TestEnforceGPU_AggregateMemoryTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{GPUMemoryGiB: 400}); err != nil {
		t.Fatalf("set: %v", err)
	}
	host, err := f.a.RegisterHost(RegisterHostSpec{Hostname: "h-gpu", Endpoint: "tcp://h-gpu:1"})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	h200 := []GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 1}}
	// Three H200 already in the project — 3 × 141 = 423 GiB > 400 GiB
	// cap on its own, but the seed bypasses enforcement to set up
	// the "already-overcommitted-on-aggregate-mem" state. Even at
	// 282 GiB (2 × 141) the cap holds for a 3rd 141-GiB admission ;
	// we use the more decisive 3-VM seed to also pin the math.
	seedGPUVM(t, f, host.UUID, "vm1", h200)
	seedGPUVM(t, f, host.UUID, "vm2", h200)
	seedGPUVM(t, f, host.UUID, "vm3", h200)
	// Fourth H200 : alloc=423 + delta=141 > cap=400.
	err = f.a.EnforceTenantQuotaForGPU(f.projUUID, h200)
	if err == nil {
		t.Fatal("expected ResourceExhausted on aggregate gpu_memory_gib, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("got code %s, want ResourceExhausted", status.Code(err))
	}
	if !contains(err.Error(), "gpu_memory_gib") {
		t.Errorf("error %q should mention gpu_memory_gib", err.Error())
	}
}

// TestEnforceGPU_UnknownModelFallsBackToZeroMemory pins the
// fallback : a request for an unknown SKU (e.g. "L40S", absent
// from the gpuModelMemoryGiB table per [[openweft_gpu_fleet]])
// contributes 0 to the memory sum, so the gpu_memory_gib cap
// can't catch the request — but the operator can still cap by
// gpu_count. The two halves of this test pin both behaviours so
// the fallback doesn't regress into "unknown SKUs silently
// bypass every GPU cap".
func TestEnforceGPU_UnknownModelFallsBackToZeroMemory(t *testing.T) {
	f := newQuotaFixture(t)
	// A tiny memory cap that 1 known H200 would breach but an
	// unknown SKU should sail past (memory contribution is 0).
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{GPUMemoryGiB: 10}); err != nil {
		t.Fatalf("set memory cap: %v", err)
	}
	if err := f.a.EnforceTenantQuotaForGPU(f.projUUID, []GPURequest{
		{Vendor: GPUVendorNVIDIA, Model: "L40S", Count: 4},
	}); err != nil {
		t.Errorf("unknown SKU should contribute 0 GiB, got %v", err)
	}
	// Same unknown SKU against a gpu_count cap : the count
	// dimension still applies (4 > 2).
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{GPUCount: 2}); err != nil {
		t.Fatalf("set count cap: %v", err)
	}
	err := f.a.EnforceTenantQuotaForGPU(f.projUUID, []GPURequest{
		{Vendor: GPUVendorNVIDIA, Model: "L40S", Count: 4},
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("unknown SKU still capped by gpu_count, got %v", err)
	}
}

// TestEnforceGPU_UnlimitedAllows pins the "zero cap = unlimited"
// short-circuit : a fresh project with no quota set must allow
// any GPU request (no allocation lookup, no error path).
func TestEnforceGPU_UnlimitedAllows(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.EnforceTenantQuotaForGPU(f.projUUID, []GPURequest{
		{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 999},
	}); err != nil {
		t.Fatalf("unlimited should allow: %v", err)
	}
}

// TestVMRegistry_RequestedGPUsRoundTrip pins the HCL persistence
// of the new VM.RequestedGPUs field : seed a VM with two GPU
// requests, reload the registry from the same storage, and
// confirm the slice round-trips verbatim. Back-compat with old
// blocks (where the field is absent) is implicit — the load path
// treats no `requested_gpu` block as nil RequestedGPUs.
func TestVMRegistry_RequestedGPUsRoundTrip(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadVMRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []GPURequest{
		{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 2, MIGSlice: "1g.10gb"},
		{Vendor: GPUVendorNVIDIA, Model: "RTX-6000-Ada", Count: 1},
	}
	v, err := reg.create(CreateVMSpec{
		ProjectUUID:   "p-1",
		Name:          "gpu-vm",
		HostUUID:      "h-1",
		RequestedGPUs: want,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reg2, err := loadVMRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(v.UUID)
	if !ok {
		t.Fatal("vm missing after reload")
	}
	if len(got.RequestedGPUs) != len(want) {
		t.Fatalf("RequestedGPUs len = %d, want %d", len(got.RequestedGPUs), len(want))
	}
	for i := range want {
		if got.RequestedGPUs[i] != want[i] {
			t.Errorf("RequestedGPUs[%d] = %+v, want %+v", i, got.RequestedGPUs[i], want[i])
		}
	}
}

// seedPCIVM is the PCI cousin of seedGPUVM : drops one VM with the
// given RequestedPCI into the registry under f.projUUID, bypassing
// the driver-handle plumbing. The aggregate enforcement reads
// vmRegistry directly, so a direct create is fine for the
// arithmetic the tests pin.
func seedPCIVM(t *testing.T, f quotaFixture, hostUUID, name string, devs []PCIRequest) {
	t.Helper()
	if _, err := f.a.vmReg.create(CreateVMSpec{
		ProjectUUID:  f.projUUID,
		Name:         name,
		HostUUID:     hostUUID,
		RequestedPCI: devs,
	}); err != nil {
		t.Fatalf("seed vm %q: %v", name, err)
	}
}

// TestTenantQuota_PCICountRoundTrip pins the pci_count dimension
// in the on-disk HCL shape : set + reload via a fresh Adapter
// against the same state dir must surface the same value.
// Mirrors TestTenantQuota_PersistsAcrossLoad for the new axis.
func TestTenantQuota_PCICountRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a1 := New(dir).(*Adapter)
	p, _, err := a1.CreateProject("alpha")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	want := TenantQuota{PCICount: 7}
	if err := a1.SetTenantQuota(p.UUID, want); err != nil {
		t.Fatalf("SetTenantQuota: %v", err)
	}
	a2 := New(dir).(*Adapter)
	if got := a2.TenantQuota(p.UUID); got != want {
		t.Errorf("after reload: got %+v, want %+v", got, want)
	}
}

// TestEnforcePCI_AggregateCountTrips pins the aggregate path :
// three 1-PCI VMs already in the project + a fourth 1-PCI VM
// admission against a pci_count=3 cap must trip. The per-request
// path alone would miss this (every individual request 1 ≤ 3).
func TestEnforcePCI_AggregateCountTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{PCICount: 3}); err != nil {
		t.Fatalf("set: %v", err)
	}
	host, err := f.a.RegisterHost(RegisterHostSpec{Hostname: "h-pci", Endpoint: "tcp://h-pci:1"})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	// Three 1-PCI VMs already in the project — total = 3, exactly
	// at cap.
	one := []PCIRequest{{VendorID: "8086", DeviceID: "1572", Count: 1}}
	seedPCIVM(t, f, host.UUID, "vm1", one)
	seedPCIVM(t, f, host.UUID, "vm2", one)
	seedPCIVM(t, f, host.UUID, "vm3", one)
	// Re-checking with nil delta stays clean (3 ≤ 3).
	if err := f.a.EnforceTenantQuotaForPCI(f.projUUID, nil); err != nil {
		t.Errorf("at-cap re-check should fit: %v", err)
	}
	// Fourth 1-PCI admission must trip : 3 + 1 > 3.
	err = f.a.EnforceTenantQuotaForPCI(f.projUUID, one)
	if err == nil {
		t.Fatal("expected ResourceExhausted on aggregate pci_count, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("got code %s, want ResourceExhausted", status.Code(err))
	}
	if !contains(err.Error(), "pci_count") {
		t.Errorf("error %q should mention pci_count", err.Error())
	}
}

// TestEnforcePCI_PerRequestTrips pins the per-request path on
// an empty project : a single VM asking for 8 PCI devices against
// a pci_count=4 cap must fail without any pre-seeded VMs.
func TestEnforcePCI_PerRequestTrips(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.SetTenantQuota(f.projUUID, TenantQuota{PCICount: 4}); err != nil {
		t.Fatalf("set: %v", err)
	}
	// No VMs registered → alloc=0 ; the request alone (8) breaches.
	err := f.a.EnforceTenantQuotaForPCI(f.projUUID, []PCIRequest{
		{VendorID: "8086", DeviceID: "1572", Count: 8},
	})
	if err == nil {
		t.Fatal("expected ResourceExhausted on per-request pci_count, got nil")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("got code %s, want ResourceExhausted", status.Code(err))
	}
}

// TestEnforcePCI_UnlimitedAllows pins the "zero cap = unlimited"
// short-circuit : a fresh project with no quota set must allow
// any PCI request (no allocation lookup, no error path).
func TestEnforcePCI_UnlimitedAllows(t *testing.T) {
	f := newQuotaFixture(t)
	if err := f.a.EnforceTenantQuotaForPCI(f.projUUID, []PCIRequest{
		{VendorID: "8086", DeviceID: "1572", Count: 999},
	}); err != nil {
		t.Fatalf("unlimited should allow: %v", err)
	}
}

// TestVMRegistry_RequestedPCIRoundTrip pins the HCL persistence
// of the new VM.RequestedPCI field : seed a VM with two PCI
// requests, reload the registry from the same storage, and
// confirm the slice round-trips verbatim. Back-compat with old
// blocks (where the field is absent) is implicit — the load path
// treats no `requested_pci` block as nil RequestedPCI.
func TestVMRegistry_RequestedPCIRoundTrip(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadVMRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []PCIRequest{
		{VendorID: "8086", DeviceID: "1572", Count: 2},
		{VendorID: "10de", DeviceID: "1eb8", Count: 1},
	}
	v, err := reg.create(CreateVMSpec{
		ProjectUUID:  "p-1",
		Name:         "pci-vm",
		HostUUID:     "h-1",
		RequestedPCI: want,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	reg2, err := loadVMRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.lookupByUUID(v.UUID)
	if !ok {
		t.Fatal("vm missing after reload")
	}
	if len(got.RequestedPCI) != len(want) {
		t.Fatalf("RequestedPCI len = %d, want %d", len(got.RequestedPCI), len(want))
	}
	for i := range want {
		if got.RequestedPCI[i] != want[i] {
			t.Errorf("RequestedPCI[%d] = %+v, want %+v", i, got.RequestedPCI[i], want[i])
		}
	}
}
