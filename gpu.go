package weft

// gpu.go owns the GPU dimension that overlays the existing
// AZ / Rack / Host axes the scheduler already understands. Two
// independent concerns live here :
//
//   * Inventory  — what GPUs does a host have ? The Host registry
//     now carries a `gpus []GPU` slice populated by the agent at
//     registration time. Today the populator (`detectGPUs`) is a
//     static stub : real detection (nvidia-smi parsing + sysfs
//     walk) is a follow-up explicitly documented in
//     docs/operations/gpu-scheduling.md.
//
//   * Request    — what does a VM need ? Two layers :
//        - `ScheduleRequest.GPU` is the **single-axis** filter that
//          mirrors the AZ / Rack axes : empty = no preference,
//          "h200" / "rtx-6000-ada" / "any-nvidia" / "none" matches
//          host inventory. This is the axis a SchedulingRule
//          declares — same shape as the existing per-rule labels.
//        - `ScheduleRequest.RequestedGPUs` is the **fine-grained**
//          per-VM request : vendor + model + count + optional MIG
//          slice. A VM with one or more entries only places on a
//          host whose inventory satisfies every entry. Matching is
//          "this host carries at least the requested SKUs" — no
//          *exclusive* pinning (operator concern, see docs).
//
// Per [[openweft_gpu_fleet]] (memory) the only models the codebase
// names are NVIDIA H200 (datacenter, MIG-capable) and NVIDIA RTX
// 6000 Ada (workstation, whole-card). Other SKUs (L40S / H100 /
// A100) are deliberately absent from examples — the GPU strings
// are open (operator-defined) but the canonical fleet stays the
// two SKUs above.

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GPU is one physical accelerator the host carries. Populated by
// the agent at registration time via `detectGPUs` ; consumed by
// the scheduler when matching `ScheduleRequest.GPU` and
// `ScheduleRequest.RequestedGPUs`.
//
// Vendor is the lowercase short tag ("nvidia"). Model is the SKU
// string in the canonical form used by SchedulingRule's `gpu`
// axis : "H200", "RTX-6000-Ada". Memory_GiB is the per-card
// memory (HBM3e for H200, GDDR6 for RTX 6000 Ada). MIG_Capable
// reports whether the GPU supports NVIDIA Multi-Instance GPU
// slicing — true for H200, false for RTX 6000 Ada.
type GPU struct {
	Vendor     string `json:"vendor"`                // "nvidia"
	Model      string `json:"model"`                 // "H200" / "RTX-6000-Ada"
	MemoryGiB  int    `json:"memory_gib,omitempty"`  // per-card memory
	MIGCapable bool   `json:"mig_capable,omitempty"` // H200 yes, RTX 6000 Ada no
}

// GPURequest is what one VM asks for. Vendor is required (the only
// supported value today is "nvidia") ; Model can be the explicit
// SKU ("H200" / "RTX-6000-Ada") or the wildcard sentinel
// `GPUModelAny` ("any") which means "any model from this vendor".
// Count defaults to 1 when zero. MIGSlice is the optional slice
// profile ("1g.10gb", "2g.20gb", "3g.40gb") — non-empty only when
// the request targets an MIG-capable card.
type GPURequest struct {
	Vendor   string `json:"vendor"`
	Model    string `json:"model,omitempty"`
	Count    int    `json:"count,omitempty"`
	MIGSlice string `json:"mig_slice,omitempty"`
}

// GPUVendorNVIDIA is the only vendor the fleet supports today.
// Kept as a const so call sites + tests stop free-styling the
// case + spelling.
const GPUVendorNVIDIA = "nvidia"

// GPUModelAny is the wildcard model — matches every model from
// the requested vendor. Used by the SchedulingRule `gpu="any-nvidia"`
// axis form.
const GPUModelAny = "any"

// GPUAxisNone is the SchedulingRule axis value meaning "this rule
// only matches hosts WITHOUT a GPU" — symmetrical to "any-nvidia"
// for the opposite intent.
const GPUAxisNone = "none"

// detectGPUs is the agent-side populator for Host.GPUs.
//
// **Today this is a static stub returning an empty slice.** Real
// detection (sysfs walk of /sys/class/drm/card*/device/vendor +
// nvidia-smi --query-gpu=name,memory.total,mig.mode.current
// --format=csv,noheader,nounits) is a follow-up tracked in
// docs/operations/gpu-scheduling.md. Operators staging GPU hosts
// today set the inventory via the static RegisterHostSpec.GPUs
// path (cluster.hcl `gpu { … }` blocks → seeded into the registry
// at first boot).
//
// The function signature is stable across the future swap — when
// detection lands, this same function gets a body and every
// call site (currently one, `selfRegisterHost`) keeps working.
func detectGPUs() []GPU {
	// Static stub. See docs/operations/gpu-scheduling.md for the
	// follow-up that wires real detection.
	return nil
}

// cloneGPUs deep-copies a GPU slice so the registry can't be
// mutated through a caller's spec pointer. Same pattern as
// cloneHostDrivers — nil/empty in → nil out so the JSON
// omitempty + HCL omit-when-empty contract stays clean.
func cloneGPUs(src []GPU) []GPU {
	if len(src) == 0 {
		return nil
	}
	out := make([]GPU, len(src))
	copy(out, src)
	return out
}

// gpuAxisMatches reports whether a host's inventory satisfies a
// single SchedulingRule `gpu` axis value. Recognised forms :
//
//   - ""              : no constraint (always matches)
//   - "none"          : host must have NO GPUs
//   - "any-nvidia"    : host must have at least one NVIDIA GPU
//   - "<model>"       : case-insensitive match on Model ("h200",
//     "rtx-6000-ada")
//
// The axis is a *filter*, not a request : it doesn't claim
// resources. Two VMs with `gpu="h200"` will both schedule on a
// single H200 host — exclusive pinning is the operator's
// responsibility (see docs/operations/gpu-scheduling.md).
func gpuAxisMatches(axis string, gpus []GPU) bool {
	if axis == "" {
		return true
	}
	switch strings.ToLower(axis) {
	case GPUAxisNone:
		return len(gpus) == 0
	case "any-nvidia":
		for _, g := range gpus {
			if strings.EqualFold(g.Vendor, GPUVendorNVIDIA) {
				return true
			}
		}
		return false
	}
	// Explicit model match — case-insensitive so operators can
	// write "h200" / "H200" interchangeably.
	want := strings.ToLower(axis)
	for _, g := range gpus {
		if strings.ToLower(g.Model) == want {
			return true
		}
	}
	return false
}

// gpuRequestSatisfied reports whether a host's inventory satisfies
// one GPURequest entry. Returns true when at least Count GPUs of
// matching Vendor+Model (Model="any" matches every model from the
// vendor) exist, and — when MIGSlice is set — they're MIG-capable.
//
// Vendor is required. An empty Vendor on the request side is
// treated as a programming error by the caller : the helper
// returns false (mismatch) rather than panicking so callers stay
// defensive. The scheduler-level validator surfaces an explicit
// error at the entry point.
func gpuRequestSatisfied(req GPURequest, gpus []GPU) bool {
	if req.Vendor == "" {
		return false
	}
	want := req.Count
	if want <= 0 {
		want = 1
	}
	matched := 0
	for _, g := range gpus {
		if !strings.EqualFold(g.Vendor, req.Vendor) {
			continue
		}
		if req.Model != "" && !strings.EqualFold(req.Model, GPUModelAny) {
			if !strings.EqualFold(g.Model, req.Model) {
				continue
			}
		}
		if req.MIGSlice != "" && !g.MIGCapable {
			continue
		}
		matched++
		if matched >= want {
			return true
		}
	}
	return false
}

// errGPUUnsatisfied returns a gRPC ResourceExhausted error when
// a scheduling request asked for a GPU axis / request that no host
// in the cluster could satisfy. Wraps the same `status.Status`
// the tenant-quotas enforcement layer uses, so the dispatch layer
// surfaces a consistent code to clients : webui can show a
// dedicated "no GPU capacity" toast rather than the generic
// scheduling failure.
func errGPUUnsatisfied(reason string) error {
	return status.Errorf(codes.ResourceExhausted, "schedule: %s", reason)
}
