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
//
// PCIBDF is the host-side PCI bus:device.function the card sits
// on (e.g. "0000:65:00.0"). Populated by `gpu_detect_linux.go`
// from the sysfs walk ; consumed by the QEMU driver to emit
// `-device vfio-pci,host=<BDF>` at StartVM time. The VZ driver
// has no GPU-passthrough path and refuses any non-empty request.
// Empty when the inventory was seeded statically from cluster.hcl
// (no BDF context at HCL-write time) — the driver surfaces a
// clear "PCI_BDF unset" error in that case so the operator
// learns the seed-vs-detected mismatch instead of silently
// booting without the GPU.
type GPU struct {
	Vendor     string `json:"vendor"`                // "nvidia"
	Model      string `json:"model"`                 // "H200" / "RTX-6000-Ada"
	MemoryGiB  int    `json:"memory_gib,omitempty"`  // per-card memory
	MIGCapable bool   `json:"mig_capable,omitempty"` // H200 yes, RTX 6000 Ada no
	PCIBDF     string `json:"pci_bdf,omitempty"`     // "0000:65:00.0" — set by detectGPUs at runtime
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

// detectGPUs is the agent-side populator for Host.GPUs. It is a
// thin platform-dispatched delegator : the Linux body lives in
// gpu_detect_linux.go (sysfs walk + nvidia-smi shell-out), every
// other OS routes to gpu_detect_other.go's stub. Build-tagged so
// the binary stays CGo-free + cross-compilable.
//
// Returns an empty slice (not an error) when detection finds
// nothing or the platform has no path — registration must not
// fail just because a host has no GPUs. Detector errors degrade
// to "log + continue with empty inventory" inside the platform
// implementation : the operator sees the diagnostic on stderr
// and the host registers as if it had no GPUs.
func detectGPUs() []GPU {
	return detectGPUsImpl()
}

// nvidiaModelMap is the case-insensitive substring → canonical
// Model lookup used to normalise nvidia-smi's `name` output to
// the SchedulingRule `gpu` axis form the rest of the codebase
// uses.
//
// Per [[openweft_gpu_fleet]] the supported fleet is **H200 +
// RTX 6000 Ada only** ; everything else is intentionally absent
// (no L40S / H100 / A100 in examples). Unknown SKUs are NOT
// rejected — they pass through verbatim so newer hardware isn't
// blocked on a code change. Callers that want to gate on
// recognised SKUs use `canonicalGPUModel` directly and check
// the second return value.
//
// Substring keys are intentional : real nvidia-smi `name` strings
// carry trailing variant qualifiers ("NVIDIA H200 80GB HBM3",
// "NVIDIA RTX 6000 Ada Generation") that must all map onto the
// short canonical form.
var nvidiaModelMap = []struct {
	substring string
	canonical string
	migCap    bool
	memGiB    int
}{
	{"H200", "H200", true, 141},
	{"RTX 6000 Ada", "RTX-6000-Ada", false, 48},
}

// canonicalGPUModel maps a vendor-reported model string to the
// canonical SchedulingRule form. The second return value is true
// when the input matched a known SKU, false otherwise — callers
// can warn-and-skip unknown SKUs by checking it. When the model
// is unknown the raw input is returned verbatim (case preserved)
// so operators staging exotic hardware aren't blocked.
func canonicalGPUModel(raw string) (model string, migCapable bool, defaultMemGiB int, known bool) {
	r := strings.ToLower(raw)
	for _, e := range nvidiaModelMap {
		if strings.Contains(r, strings.ToLower(e.substring)) {
			return e.canonical, e.migCap, e.memGiB, true
		}
	}
	return raw, false, 0, false
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
