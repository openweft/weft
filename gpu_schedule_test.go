package weft

import (
	"context"
	"testing"
)

func TestSelectGPUClaims_CrossRequestContention(t *testing.T) {
	// One card, requested twice in the same VM spec → the second entry
	// must NOT re-pick the card the first already took.
	h := Host{UUID: "h1", GPUs: []GPU{{Vendor: GPUVendorNVIDIA, Model: "RTX-6000-Ada", PCIBDF: "0000:01:00.0"}}}
	reqs := []GPURequest{
		{Vendor: GPUVendorNVIDIA, Count: 1},
		{Vendor: GPUVendorNVIDIA, Count: 1},
	}
	if _, ok := selectGPUClaims(reqs, h, "vm-a", 0, func(string) bool { return false }); ok {
		t.Fatal("two count=1 requests can't both be satisfied by a single card")
	}
	// A single count=1 request fits and yields exactly one whole-card claim.
	claims, ok := selectGPUClaims(reqs[:1], h, "vm-a", 42, func(string) bool { return false })
	if !ok || len(claims) != 1 {
		t.Fatalf("want 1 claim, ok=%v claims=%d", ok, len(claims))
	}
	if claims[0].Kind != GPUClaimWholeCard || claims[0].ResourceID != "0000:01:00.0" || claims[0].CreatedAtUnixNs != 42 {
		t.Fatalf("unexpected claim: %+v", claims[0])
	}
}

func TestSelectGPUClaims_MIGPicksDistinctSlices(t *testing.T) {
	h := migH200Host("dc1", 1, 4, "1g.18gb")
	claims, ok := selectGPUClaims(
		[]GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 3, MIGSlice: "1g.18gb"}},
		h, "vm-a", 0, func(string) bool { return false },
	)
	if !ok || len(claims) != 3 {
		t.Fatalf("want 3 MIG claims, ok=%v n=%d", ok, len(claims))
	}
	seen := map[string]bool{}
	for _, c := range claims {
		if c.Kind != GPUClaimMIG {
			t.Fatalf("expected MIG claim, got %+v", c)
		}
		if seen[c.ResourceID] {
			t.Fatalf("duplicate MIG slice picked: %s", c.ResourceID)
		}
		seen[c.ResourceID] = true
	}
}

// TestScheduleVMExclusive_EndToEnd exercises the full Adapter path:
// placement claims hardware, a second VM is refused, and unregistering
// the first frees the card for the second.
func TestScheduleVMExclusive_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)
	ctx := context.Background()

	// A single-H200 host (whole-card, MIG off). BDF set as detection would.
	if _, err := a.RegisterHost(RegisterHostSpec{
		Hostname:     "gpu-1",
		Architecture: "amd64",
		Hypervisor:   "qemu",
		GPUs: []GPU{{
			Vendor: GPUVendorNVIDIA, Model: "H200", MemoryGiB: 141,
			MIGCapable: true, PCIBDF: "0000:65:00.0", NVLinkDomain: "nvl4-a",
		}},
	}); err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}

	req := ScheduleRequest{
		Architecture:  "amd64",
		Hypervisor:    "qemu",
		RequestedGPUs: []GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 1}},
	}

	// First VM claims the card.
	h, claims, err := a.ScheduleVMExclusive(ctx, req, "vm-a", 1)
	if err != nil {
		t.Fatalf("first placement: %v", err)
	}
	if h.Hostname != "gpu-1" || len(claims) != 1 || claims[0].ResourceID != "0000:65:00.0" {
		t.Fatalf("unexpected first placement: host=%s claims=%+v", h.Hostname, claims)
	}

	// Second VM with the same request → no unclaimed capacity.
	if _, _, err := a.ScheduleVMExclusive(ctx, req, "vm-b", 2); err == nil {
		t.Fatal("second placement must fail — the only H200 is claimed")
	}

	// Free vm-a's claim (what UnregisterVM does) → the card is schedulable.
	if freed := a.gpuClaims.ReleaseVM("vm-a"); freed != 1 {
		t.Fatalf("ReleaseVM(vm-a) want 1 freed, got %d", freed)
	}
	if _, _, err := a.ScheduleVMExclusive(ctx, req, "vm-b", 3); err != nil {
		t.Fatalf("placement after release should succeed: %v", err)
	}
}

func TestScheduleVMExclusive_BranchCoverage(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)
	ctx := context.Background()

	// Empty vmUUID → error.
	if _, _, err := a.ScheduleVMExclusive(ctx, ScheduleRequest{}, "", 0); err == nil {
		t.Fatal("empty vmUUID must error")
	}

	// No RequestedGPUs → delegates to ScheduleVM, returns no claims. The
	// self-registered local host satisfies an unconstrained request.
	h, claims, err := a.ScheduleVMExclusive(ctx, ScheduleRequest{}, "vm-x", 0)
	if err != nil {
		t.Fatalf("no-GPU delegation: %v", err)
	}
	if claims != nil || h.UUID == "" {
		t.Fatalf("delegation should return a host and nil claims, got host=%q claims=%v", h.UUID, claims)
	}

	// A GPU host whose generic-PCI request can't be met → skipped → exhausted.
	if _, err := a.RegisterHost(RegisterHostSpec{
		Hostname: "gpu-2", Architecture: "amd64", Hypervisor: "qemu",
		GPUs: []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200", PCIBDF: "0000:65:00.0"}},
	}); err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	req := ScheduleRequest{
		Architecture:  "amd64",
		Hypervisor:    "qemu",
		RequestedGPUs: []GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 1}},
		RequestedPCI:  []PCIRequest{{VendorID: "8086", DeviceID: "dead", Count: 1}},
	}
	if _, _, err := a.ScheduleVMExclusive(ctx, req, "vm-y", 0); err == nil {
		t.Fatal("unsatisfiable PCI request must yield no placement")
	}

	// GPU axis mismatch (asks for rtx-6000-ada, host has H200) → exhausted.
	axisReq := ScheduleRequest{
		Architecture:  "amd64",
		Hypervisor:    "qemu",
		GPU:           "rtx-6000-ada",
		RequestedGPUs: []GPURequest{{Vendor: GPUVendorNVIDIA, Count: 1}},
	}
	if _, _, err := a.ScheduleVMExclusive(ctx, axisReq, "vm-z", 0); err == nil {
		t.Fatal("GPU-axis mismatch must yield no placement")
	}
}

// TestUnregisterVM_ReleasesGPUClaims verifies the live release hook:
// removing a VM from the inventory frees the GPU it held.
func TestUnregisterVM_ReleasesGPUClaims(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	// Seed a VM in the registry directly (RegisterVM needs a live driver
	// handle, out of scope here) and a claim keyed by its UUID.
	v, err := a.vmReg.create(CreateVMSpec{Name: "vm-a", ProjectUUID: "p1", HostUUID: "h1"})
	if err != nil {
		t.Fatalf("vmReg.create: %v", err)
	}
	if err := a.gpuClaims.Claim(GPUClaim{
		HostUUID: "h1", ResourceID: "0000:65:00.0", Kind: GPUClaimWholeCard, VMUUID: v.UUID,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := a.UnregisterVM(v.UUID); err != nil {
		t.Fatalf("UnregisterVM: %v", err)
	}
	if a.gpuClaims.IsClaimed("h1", "0000:65:00.0") {
		t.Fatal("UnregisterVM must release the VM's GPU claim")
	}
}
