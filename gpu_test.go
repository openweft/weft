package weft

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// h200Host is a helper that mirrors activeHost but pre-seeds an
// H200 GPU inventory. Used by the GPU-axis tests below so the
// candidate-shaping noise stays out of the test bodies.
func h200Host(uuid string, count int) Host {
	return activeHost(uuid, func(h *Host) {
		h.GPUs = make([]GPU, 0, count)
		for i := 0; i < count; i++ {
			h.GPUs = append(h.GPUs, GPU{
				Vendor:     GPUVendorNVIDIA,
				Model:      "H200",
				MemoryGiB:  141,
				MIGCapable: true,
			})
		}
	})
}

// rtx6000AdaHost — workstation SKU per [[openweft_gpu_fleet]] :
// whole-card allocation, NOT MIG-capable.
func rtx6000AdaHost(uuid string, count int) Host {
	return activeHost(uuid, func(h *Host) {
		h.GPUs = make([]GPU, 0, count)
		for i := 0; i < count; i++ {
			h.GPUs = append(h.GPUs, GPU{
				Vendor:     GPUVendorNVIDIA,
				Model:      "RTX-6000-Ada",
				MemoryGiB:  48,
				MIGCapable: false,
			})
		}
	})
}

// TestHostRegistry_GPUInventoryRoundtrip pins gate #1 : the GPU
// slice survives a register → save → reload cycle without
// drift. Covers the HCL block <-> Go struct mapping in both
// directions.
func TestHostRegistry_GPUInventoryRoundtrip(t *testing.T) {
	storage := NewMemStorage()
	reg, err := loadHostRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("loadHostRegistry: %v", err)
	}
	specGPUs := []GPU{
		{Vendor: GPUVendorNVIDIA, Model: "H200", MemoryGiB: 141, MIGCapable: true},
		{Vendor: GPUVendorNVIDIA, Model: "H200", MemoryGiB: 141, MIGCapable: true},
	}
	h, err := reg.register(RegisterHostSpec{
		Hostname:     "gpu-01",
		AZ:           "us-east-1a",
		Hypervisor:   "qemu",
		Architecture: "amd64",
		GPUs:         specGPUs,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(h.GPUs) != 2 {
		t.Fatalf("register dropped GPU entries: got %d, want 2", len(h.GPUs))
	}
	// Mutate the caller-side spec slice : the registry must have
	// its own copy (cloneGPUs contract) so a downstream caller
	// that holds onto its RegisterHostSpec.GPUs slice can't
	// silently corrupt the in-memory state.
	specGPUs[0].Model = "Tampered"
	got, _ := reg.lookupByUUID(h.UUID)
	if got.GPUs[0].Model != "H200" {
		t.Errorf("cloneGPUs failed : registry mutated through caller spec slice (got %q)", got.GPUs[0].Model)
	}

	// Reload from the persisted blob and check the GPU slice round-tripped.
	reg2, err := loadHostRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("loadHostRegistry (reload): %v", err)
	}
	reloaded, ok := reg2.lookupByUUID(h.UUID)
	if !ok {
		t.Fatalf("host %q missing after reload", h.UUID)
	}
	if len(reloaded.GPUs) != 2 {
		t.Fatalf("reloaded GPU count = %d, want 2", len(reloaded.GPUs))
	}
	for i, g := range reloaded.GPUs {
		if g.Vendor != GPUVendorNVIDIA || g.Model != "H200" || g.MemoryGiB != 141 || !g.MIGCapable {
			t.Errorf("reloaded GPU[%d] drifted: %+v", i, g)
		}
	}
}

// TestScheduler_GPUAxis_Match pins gate #2a : a SchedulingRule
// asking for `gpu="h200"` picks the H200 host and skips the
// no-GPU + RTX 6000 Ada hosts.
func TestScheduler_GPUAxis_Match(t *testing.T) {
	candidates := []Host{
		activeHost("plain"), // no GPU
		rtx6000AdaHost("wks", 1),
		h200Host("dc", 4),
	}
	got, err := FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{GPU: "h200"}, candidates)
	if err != nil {
		t.Fatalf("schedule with gpu=h200: %v", err)
	}
	if got.UUID != "dc" {
		t.Errorf("gpu=h200 axis should pick the H200 host, got %q", got.UUID)
	}
	// any-nvidia matches the *first* GPU host in candidate order.
	got, err = FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{GPU: "any-nvidia"}, candidates)
	if err != nil {
		t.Fatalf("schedule with gpu=any-nvidia: %v", err)
	}
	if got.UUID != "wks" {
		t.Errorf("gpu=any-nvidia should pick first NVIDIA host (wks), got %q", got.UUID)
	}
	// gpu="none" — only the GPU-less host qualifies.
	got, err = FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{GPU: "none"}, candidates)
	if err != nil {
		t.Fatalf("schedule with gpu=none: %v", err)
	}
	if got.UUID != "plain" {
		t.Errorf("gpu=none should pick the GPU-less host, got %q", got.UUID)
	}
}

// TestScheduler_GPUAxis_MissReturnsResourceExhausted pins gate #2b :
// `gpu="h200"` against a cluster with no H200 surfaces a gRPC
// ResourceExhausted error. Webui keys on this code to render the
// "no GPU capacity" toast.
func TestScheduler_GPUAxis_MissReturnsResourceExhausted(t *testing.T) {
	candidates := []Host{
		activeHost("plain"),
		rtx6000AdaHost("wks", 1),
	}
	_, err := FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{GPU: "h200"}, candidates)
	if err == nil {
		t.Fatal("schedule with no matching GPU should error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got %v (%v)", st.Code(), err)
	}
	if !strings.Contains(err.Error(), "GPU") {
		t.Errorf("error message should mention GPU: %v", err)
	}
}

// TestScheduler_RequestedGPU_MIGCapableFilter pins gate #3 : a
// request that names a MIG slice ("1g.10gb") only matches a host
// whose GPU is MIG-capable. The RTX 6000 Ada host has GPUs but
// isn't MIG-capable, so it must be rejected ; the H200 host
// matches.
func TestScheduler_RequestedGPU_MIGCapableFilter(t *testing.T) {
	candidates := []Host{
		rtx6000AdaHost("wks", 1), // MIG_Capable=false
		h200Host("dc", 1),        // MIG_Capable=true
	}
	req := ScheduleRequest{
		RequestedGPUs: []GPURequest{
			{Vendor: GPUVendorNVIDIA, Model: GPUModelAny, Count: 1, MIGSlice: "1g.10gb"},
		},
	}
	got, err := FirstFitScheduler{}.Schedule(context.Background(), req, candidates)
	if err != nil {
		t.Fatalf("schedule with MIG slice: %v", err)
	}
	if got.UUID != "dc" {
		t.Errorf("MIG-slice request should pick the MIG-capable host, got %q", got.UUID)
	}
	// Now run with ONLY the workstation host : must fail with
	// ResourceExhausted (no MIG-capable card to satisfy the slice).
	_, err = FirstFitScheduler{}.Schedule(context.Background(), req, []Host{rtx6000AdaHost("wks", 1)})
	if err == nil {
		t.Fatal("MIG slice on non-MIG host should fail")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted on MIG miss, got %v", err)
	}
}

// TestScheduler_RequestedGPU_VendorMismatchRejected pins gate #4 :
// a request with a vendor the host doesn't carry is rejected.
// Today the fleet is NVIDIA-only ; an "amd" vendor request must
// not silently land on an NVIDIA host.
func TestScheduler_RequestedGPU_VendorMismatchRejected(t *testing.T) {
	candidates := []Host{h200Host("dc", 2)}
	req := ScheduleRequest{
		RequestedGPUs: []GPURequest{
			{Vendor: "amd", Model: GPUModelAny, Count: 1},
		},
	}
	_, err := FirstFitScheduler{}.Schedule(context.Background(), req, candidates)
	if err == nil {
		t.Fatal("vendor mismatch should be rejected")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted on vendor mismatch, got %v", err)
	}
}

// TestScheduler_RequestedGPU_CountExceedsInventory pins the count
// dimension : asking for 4 H200s on a host with 2 must fail
// (even though vendor + model match). Surfaces the same
// ResourceExhausted code so webui doesn't have to special-case
// the "want more than the host has" path.
func TestScheduler_RequestedGPU_CountExceedsInventory(t *testing.T) {
	candidates := []Host{h200Host("dc", 2)}
	req := ScheduleRequest{
		RequestedGPUs: []GPURequest{
			{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 4},
		},
	}
	_, err := FirstFitScheduler{}.Schedule(context.Background(), req, candidates)
	if err == nil {
		t.Fatal("count=4 on a 2-GPU host should fail")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted on count miss, got %v", err)
	}
	// Count=2 on the same host must succeed.
	req.RequestedGPUs[0].Count = 2
	got, err := FirstFitScheduler{}.Schedule(context.Background(), req, candidates)
	if err != nil {
		t.Fatalf("count=2 on 2-GPU host should succeed: %v", err)
	}
	if got.UUID != "dc" {
		t.Errorf("matching count should pick the H200 host, got %q", got.UUID)
	}
}

// TestScheduler_GPUAxis_LabelSelectorStillEnforced pins the
// "GPU is one dimension among others" contract per
// [[openweft_nominal_binding]] : a GPU axis match doesn't let
// the request bypass label selectors / AZ / capability filters.
func TestScheduler_GPUAxis_LabelSelectorStillEnforced(t *testing.T) {
	candidates := []Host{
		activeHost("h200-wrong-az", func(h *Host) {
			h.AZ = "us-west-2c"
			h.GPUs = []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200", MIGCapable: true}}
		}),
		activeHost("h200-right-az", func(h *Host) {
			h.AZ = "us-east-1a"
			h.GPUs = []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200", MIGCapable: true}}
		}),
	}
	got, err := FirstFitScheduler{}.Schedule(context.Background(),
		ScheduleRequest{GPU: "h200", AZ: "us-east-1a"}, candidates)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if got.UUID != "h200-right-az" {
		t.Errorf("AZ filter should still apply alongside GPU axis, got %q", got.UUID)
	}
}

// TestGPURequestSatisfied_EmptyVendorRejected pins the defensive
// path : a request with an empty vendor field is treated as a
// programming error and matches no host, rather than wildcarding.
func TestGPURequestSatisfied_EmptyVendorRejected(t *testing.T) {
	gpus := []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200"}}
	if gpuRequestSatisfied(GPURequest{Model: "H200", Count: 1}, gpus) {
		t.Error("empty Vendor should not silently wildcard")
	}
}

// TestCanonicalGPUModel pins the model-string normaliser. nvidia-smi
// returns variant-qualified names ("NVIDIA H200 80GB HBM3", etc.)
// that must collapse onto the canonical SchedulingRule form. Unknown
// SKUs pass through verbatim with `known=false` so the agent can
// warn-and-skip the strict path while still recording inventory.
func TestCanonicalGPUModel(t *testing.T) {
	cases := []struct {
		in        string
		wantModel string
		wantMIG   bool
		wantMem   int
		wantKnown bool
	}{
		{"NVIDIA H200 80GB HBM3", "H200", true, 141, true},
		{"NVIDIA H200", "H200", true, 141, true},
		{"NVIDIA RTX 6000 Ada Generation", "RTX-6000-Ada", false, 48, true},
		{"NVIDIA L40S", "NVIDIA L40S", false, 0, false}, // unknown — verbatim + known=false
		{"", "", false, 0, false},
	}
	for _, tc := range cases {
		gotModel, gotMIG, gotMem, gotKnown := canonicalGPUModel(tc.in)
		if gotModel != tc.wantModel || gotMIG != tc.wantMIG || gotMem != tc.wantMem || gotKnown != tc.wantKnown {
			t.Errorf("canonicalGPUModel(%q) = (%q, %v, %d, %v), want (%q, %v, %d, %v)",
				tc.in, gotModel, gotMIG, gotMem, gotKnown,
				tc.wantModel, tc.wantMIG, tc.wantMem, tc.wantKnown)
		}
	}
}
