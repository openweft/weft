package weft

import (
	"context"
	"testing"
)

func TestGPUClaimRecord_RoundTrip(t *testing.T) {
	in := GPUClaim{
		HostUUID:        "host-1",
		ResourceID:      "0000:65:00.0",
		Kind:            GPUClaimWholeCard,
		VMUUID:          "vm-7",
		Model:           "H200",
		CreatedAtUnixNs: 1750000000123456789, // exceeds cty safe-int range → stored as string
	}
	out, err := decodeGPUClaimRecord(encodeGPUClaimRecord(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestGPUAllocTable_KVPersistAndReload(t *testing.T) {
	ctx := context.Background()
	kv := NewMemKVStorage("/weft/gpu_allocations")
	tbl := newGPUAllocTableKV(kv)

	// A whole-card claim and a MIG claim on two hosts.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	must(tbl.Claim(GPUClaim{HostUUID: "h1", ResourceID: "0000:65:00.0", Kind: GPUClaimWholeCard, VMUUID: "vm-a", Model: "H200"}))
	must(tbl.ClaimAll([]GPUClaim{
		{HostUUID: "h2", ResourceID: "MIG-x-0", Kind: GPUClaimMIG, VMUUID: "vm-b", Model: "H200"},
		{HostUUID: "h2", ResourceID: "MIG-x-1", Kind: GPUClaimMIG, VMUUID: "vm-b", Model: "H200"},
	}))

	// Reload from the same KV — every claim must come back.
	re, err := loadGPUAllocTableKV(ctx, kv)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !re.IsClaimed("h1", "0000:65:00.0") {
		t.Fatal("whole-card claim lost on reload")
	}
	if !re.IsClaimed("h2", "MIG-x-0") || !re.IsClaimed("h2", "MIG-x-1") {
		t.Fatal("MIG claims lost on reload")
	}
	// byVM index rebuilt → ReleaseVM works post-reload.
	if freed := re.ReleaseVM("vm-b"); freed != 2 {
		t.Fatalf("reloaded ReleaseVM(vm-b) want 2, got %d", freed)
	}

	// Release on the original table must also delete the KV record, so a
	// fresh reload no longer sees it.
	if !tbl.Release("h1", "0000:65:00.0") {
		t.Fatal("Release should remove the claim")
	}
	re2, err := loadGPUAllocTableKV(ctx, kv)
	if err != nil {
		t.Fatalf("reload 2: %v", err)
	}
	if re2.IsClaimed("h1", "0000:65:00.0") {
		t.Fatal("released claim must not survive reload")
	}
}

func TestGPUClaimRecord_DecodeErrors(t *testing.T) {
	if _, err := decodeGPUClaimRecord([]byte("this is not hcl {{{")); err == nil {
		t.Fatal("malformed HCL must error")
	}
	// Zero blocks → error (the per-record contract is exactly one).
	if _, err := decodeGPUClaimRecord([]byte("")); err == nil {
		t.Fatal("empty document (0 blocks) must error")
	}
}

func TestLoadGPUAllocTableKV_SkipsCorruptAndInvalid(t *testing.T) {
	ctx := context.Background()
	kv := NewMemKVStorage("/weft/gpu_allocations")
	// One good record, one corrupt blob, one structurally-valid-but-
	// incomplete claim (missing vm_uuid) — only the good one survives.
	_ = kv.PutOne(ctx, "h1~good", encodeGPUClaimRecord(GPUClaim{
		HostUUID: "h1", ResourceID: "good", Kind: GPUClaimMIG, VMUUID: "vm-a",
	}))
	_ = kv.PutOne(ctx, "h1~corrupt", []byte("garbage {"))
	_ = kv.PutOne(ctx, "h1~novm", encodeGPUClaimRecord(GPUClaim{
		HostUUID: "h1", ResourceID: "novm", Kind: GPUClaimMIG, VMUUID: "x",
	})[:10]) // truncated → decode error

	tbl, err := loadGPUAllocTableKV(ctx, kv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !tbl.IsClaimed("h1", "good") {
		t.Fatal("the well-formed record must load")
	}
	if got := len(tbl.ClaimsForHost("h1")); got != 1 {
		t.Fatalf("only the good record should survive, got %d claims", got)
	}
}

func TestGPUAllocTable_ClaimAllValidationError(t *testing.T) {
	tbl := newGPUAllocTable()
	// Missing vm_uuid on one entry → ClaimAll rejects the whole batch.
	err := tbl.ClaimAll([]GPUClaim{
		{HostUUID: "h1", ResourceID: "r0", Kind: GPUClaimMIG, VMUUID: "vm-a"},
		{HostUUID: "h1", ResourceID: "r1", Kind: GPUClaimMIG}, // no VMUUID
	})
	if err == nil {
		t.Fatal("ClaimAll with an invalid entry must error")
	}
	if len(tbl.ClaimsForHost("h1")) != 0 {
		t.Fatal("validation failure must claim nothing")
	}
}

func TestGPUAllocTable_ClaimAllAtomicOnConflict(t *testing.T) {
	tbl := newGPUAllocTable()
	// vm-a already holds r1.
	if err := tbl.Claim(GPUClaim{HostUUID: "h1", ResourceID: "r1", Kind: GPUClaimMIG, VMUUID: "vm-a"}); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	// vm-b asks for {r0, r1, r2} — r1 conflicts, so NOTHING should be claimed.
	err := tbl.ClaimAll([]GPUClaim{
		{HostUUID: "h1", ResourceID: "r0", Kind: GPUClaimMIG, VMUUID: "vm-b"},
		{HostUUID: "h1", ResourceID: "r1", Kind: GPUClaimMIG, VMUUID: "vm-b"},
		{HostUUID: "h1", ResourceID: "r2", Kind: GPUClaimMIG, VMUUID: "vm-b"},
	})
	if err == nil {
		t.Fatal("ClaimAll with a conflicting entry must error")
	}
	if tbl.IsClaimed("h1", "r0") || tbl.IsClaimed("h1", "r2") {
		t.Fatal("ClaimAll must be all-or-nothing — no partial claims on conflict")
	}
	if len(tbl.ClaimsForHost("h1")) != 1 {
		t.Fatalf("only the pre-existing claim should remain, got %d", len(tbl.ClaimsForHost("h1")))
	}
}
