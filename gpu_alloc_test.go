package weft

import "testing"

// migH200Host mirrors h200Host (gpu_test.go) but partitions each card
// into `slicesPerCard` MIG instances of `profile`, with deterministic
// BDFs / UUIDs so the claim assertions can name a specific resource.
func migH200Host(uuid string, cards, slicesPerCard int, profile string) Host {
	h := activeHost(uuid, func(h *Host) {
		h.GPUs = make([]GPU, 0, cards)
		for c := 0; c < cards; c++ {
			bdf := mkBDF(c)
			g := GPU{
				Vendor:       GPUVendorNVIDIA,
				Model:        "H200",
				MemoryGiB:    141,
				MIGCapable:   true,
				PCIBDF:       bdf,
				NVLinkDomain: "nvl4-a",
			}
			for s := 0; s < slicesPerCard; s++ {
				g.MIGInstances = append(g.MIGInstances, MIGInstance{
					ParentBDF: bdf,
					Profile:   profile,
					GIID:      s,
					UUID:      mkMIGUUID(c, s),
					MemoryGiB: 18,
				})
			}
			h.GPUs = append(h.GPUs, g)
		}
	})
	return h
}

func mkBDF(card int) string { return "0000:65:0" + string(rune('0'+card)) + ".0" }
func mkMIGUUID(card, s int) string {
	return "MIG-" + string(rune('a'+card)) + "-" + string(rune('0'+s))
}

func TestGPUAllocTable_ClaimReleaseRoundTrip(t *testing.T) {
	tbl := newGPUAllocTable()
	c := GPUClaim{HostUUID: "h1", ResourceID: "0000:65:00.0", Kind: GPUClaimWholeCard, VMUUID: "vm-a", Model: "H200"}

	if tbl.IsClaimed("h1", "0000:65:00.0") {
		t.Fatal("resource should start unclaimed")
	}
	if err := tbl.Claim(c); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !tbl.IsClaimed("h1", "0000:65:00.0") {
		t.Fatal("resource should be claimed after Claim")
	}
	if !tbl.Release("h1", "0000:65:00.0") {
		t.Fatal("Release should report a removed claim")
	}
	if tbl.IsClaimed("h1", "0000:65:00.0") {
		t.Fatal("resource should be free after Release")
	}
	if tbl.Release("h1", "0000:65:00.0") {
		t.Fatal("releasing an unheld resource must be a no-op (false)")
	}
}

func TestGPUAllocTable_DoubleClaimRejected(t *testing.T) {
	tbl := newGPUAllocTable()
	base := GPUClaim{HostUUID: "h1", ResourceID: "0000:65:00.0", Kind: GPUClaimWholeCard, VMUUID: "vm-a"}
	if err := tbl.Claim(base); err != nil {
		t.Fatalf("claim by vm-a: %v", err)
	}
	// Different VM wanting the same resource → rejected.
	conflict := base
	conflict.VMUUID = "vm-b"
	if err := tbl.Claim(conflict); err == nil {
		t.Fatal("claim of held resource by a different VM must error")
	}
	// Same VM re-claiming → idempotent, no error, no duplicate byVM entry.
	if err := tbl.Claim(base); err != nil {
		t.Fatalf("idempotent re-claim by vm-a: %v", err)
	}
	if freed := tbl.ReleaseVM("vm-a"); freed != 1 {
		t.Fatalf("ReleaseVM should free exactly 1 claim, freed %d", freed)
	}
}

func TestGPUAllocTable_ClaimValidation(t *testing.T) {
	tbl := newGPUAllocTable()
	for _, bad := range []GPUClaim{
		{ResourceID: "r", VMUUID: "v"},   // missing host
		{HostUUID: "h", VMUUID: "v"},     // missing resource
		{HostUUID: "h", ResourceID: "r"}, // missing vm
	} {
		if err := tbl.Claim(bad); err == nil {
			t.Fatalf("expected validation error for %+v", bad)
		}
	}
}

func TestGPUAllocTable_ReleaseVMFreesAllAndIsIdempotent(t *testing.T) {
	tbl := newGPUAllocTable()
	for _, rid := range []string{"r0", "r1", "r2"} {
		if err := tbl.Claim(GPUClaim{HostUUID: "h1", ResourceID: rid, Kind: GPUClaimMIG, VMUUID: "vm-x"}); err != nil {
			t.Fatalf("claim %s: %v", rid, err)
		}
	}
	if len(tbl.ClaimsForHost("h1")) != 3 {
		t.Fatalf("want 3 claims on h1, got %d", len(tbl.ClaimsForHost("h1")))
	}
	if freed := tbl.ReleaseVM("vm-x"); freed != 3 {
		t.Fatalf("ReleaseVM want 3 freed, got %d", freed)
	}
	if got := len(tbl.ClaimsForHost("h1")); got != 0 {
		t.Fatalf("host should hold no claims after ReleaseVM, got %d", got)
	}
	if freed := tbl.ReleaseVM("vm-x"); freed != 0 {
		t.Fatalf("second ReleaseVM must be a no-op, freed %d", freed)
	}
}

func TestGPURequestSatisfiedExcl_WholeCardExhaustion(t *testing.T) {
	h := rtx6000AdaHost("wks", 2)
	// Detection would set the BDFs; the static helper doesn't, so set
	// them here — exclusivity needs a stable resource id per card.
	h.GPUs[0].PCIBDF = "0000:01:00.0"
	h.GPUs[1].PCIBDF = "0000:02:00.0"
	req := GPURequest{Vendor: GPUVendorNVIDIA, Model: "RTX-6000-Ada", Count: 1}

	tbl := newGPUAllocTable()
	none := func(string) bool { return false }
	if !gpuRequestSatisfiedExcl(req, h.GPUs, none) {
		t.Fatal("2 free cards should satisfy a count=1 request")
	}
	// Claim both cards → no free card left.
	_ = tbl.Claim(GPUClaim{HostUUID: h.UUID, ResourceID: "0000:01:00.0", Kind: GPUClaimWholeCard, VMUUID: "vm-a"})
	_ = tbl.Claim(GPUClaim{HostUUID: h.UUID, ResourceID: "0000:02:00.0", Kind: GPUClaimWholeCard, VMUUID: "vm-b"})
	if tbl.HostSatisfiesExcl([]GPURequest{req}, h) {
		t.Fatal("both cards claimed → request must NOT be satisfiable")
	}
}

func TestGPURequestSatisfiedExcl_EmptyBDFSkipped(t *testing.T) {
	// A statically-seeded card with no BDF can't be claimed exclusively,
	// so it must not count toward an exclusive request.
	h := rtx6000AdaHost("wks", 1) // helper leaves PCIBDF empty
	req := GPURequest{Vendor: GPUVendorNVIDIA, Count: 1}
	if gpuRequestSatisfiedExcl(req, h.GPUs, func(string) bool { return false }) {
		t.Fatal("card with empty BDF must not satisfy an exclusive whole-card request")
	}
	// Non-exclusive matcher still counts it (unchanged behaviour).
	if !gpuRequestSatisfied(req, h.GPUs) {
		t.Fatal("non-exclusive matcher should still match the empty-BDF card")
	}
}

func TestGPURequestSatisfiedExcl_MIGSliceCounting(t *testing.T) {
	h := migH200Host("dc1", 1 /*cards*/, 7 /*slices*/, "1g.18gb")
	req := GPURequest{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 3, MIGSlice: "1g.18gb"}
	tbl := newGPUAllocTable()

	if !tbl.HostSatisfiesExcl([]GPURequest{req}, h) {
		t.Fatal("7 free slices should satisfy a count=3 MIG request")
	}
	// Claim 5 of the 7 slices → only 2 free, can't satisfy count=3.
	for s := 0; s < 5; s++ {
		if err := tbl.Claim(GPUClaim{
			HostUUID: h.UUID, ResourceID: mkMIGUUID(0, s), Kind: GPUClaimMIG, VMUUID: "vm-" + string(rune('a'+s)),
		}); err != nil {
			t.Fatalf("claim slice %d: %v", s, err)
		}
	}
	if tbl.HostSatisfiesExcl([]GPURequest{req}, h) {
		t.Fatal("only 2 free slices left → count=3 MIG request must fail")
	}
	// A count=2 request still fits.
	smaller := req
	smaller.Count = 2
	if !tbl.HostSatisfiesExcl([]GPURequest{smaller}, h) {
		t.Fatal("2 free slices should satisfy a count=2 MIG request")
	}
}

func TestGPUAllocTable_PartialReleaseKeepsOtherClaims(t *testing.T) {
	tbl := newGPUAllocTable()
	for _, rid := range []string{"r0", "r1", "r2"} {
		if err := tbl.Claim(GPUClaim{HostUUID: "h1", ResourceID: rid, Kind: GPUClaimMIG, VMUUID: "vm-x"}); err != nil {
			t.Fatalf("claim %s: %v", rid, err)
		}
	}
	// Release the middle resource — the VM keeps the other two, exercising
	// the multi-key prune path in pruneVMKeyLocked.
	if !tbl.Release("h1", "r1") {
		t.Fatal("Release r1 should remove a claim")
	}
	if tbl.IsClaimed("h1", "r1") {
		t.Fatal("r1 should be free")
	}
	if !tbl.IsClaimed("h1", "r0") || !tbl.IsClaimed("h1", "r2") {
		t.Fatal("r0 and r2 must still be held")
	}
	if freed := tbl.ReleaseVM("vm-x"); freed != 2 {
		t.Fatalf("ReleaseVM should free the 2 remaining claims, freed %d", freed)
	}
}

func TestGPURequestSatisfiedExcl_GuardsAndEmpty(t *testing.T) {
	none := func(string) bool { return false }
	// Empty Vendor is a programming error → false, like gpuRequestSatisfied.
	if gpuRequestSatisfiedExcl(GPURequest{Count: 1}, nil, none) {
		t.Fatal("empty Vendor must not satisfy an exclusive request")
	}
	// Empty request list is vacuously satisfiable.
	if !newGPUAllocTable().HostSatisfiesExcl(nil, Host{UUID: "h1"}) {
		t.Fatal("a host with no GPU request must be satisfiable")
	}
}

func TestGPURequestSatisfiedExcl_MIGRequestIgnoresWholeCardClaims(t *testing.T) {
	// A MIG request counts MIG instances, never the parent card's BDF —
	// claiming the BDF (which detection wouldn't do for a MIG-mode card)
	// must not reduce MIG capacity.
	h := migH200Host("dc1", 1, 2, "3g.71gb")
	tbl := newGPUAllocTable()
	_ = tbl.Claim(GPUClaim{HostUUID: h.UUID, ResourceID: mkBDF(0), Kind: GPUClaimWholeCard, VMUUID: "vm-z"})
	req := GPURequest{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 2, MIGSlice: "3g.71gb"}
	if !tbl.HostSatisfiesExcl([]GPURequest{req}, h) {
		t.Fatal("whole-card claim must not affect MIG-instance capacity")
	}
}
