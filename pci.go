package weft

// pci.go owns the generic PCI passthrough surface. Sibling of
// gpu.go : GPUs already have their own dimension (own SKU
// canonicalisation, MIG semantics, SchedulingRule `gpu` axis) ;
// **non-GPU** PCI passthrough (NICs, NVMe, sound cards, FPGAs)
// gets its own surface here so the GPU file doesn't grow a
// second-class generic-PCI cousin.
//
// Two concerns live in this file :
//
//   * Inventory  — what PCI devices does a host carry beyond the
//     GPUs already in Host.GPUs ? The Host registry carries a
//     `pci_devices []PCIDevice` slice populated at registration
//     time via detectPCI() — Linux-only, walks
//     /sys/bus/pci/devices/* ; every other GOOS returns nil.
//     Operators staging non-GPU passthrough today can seed the
//     inventory via the static RegisterHostSpec.PCIDevices path
//     (cluster.hcl `pci { … }` blocks → seeded into the registry
//     at first boot).
//
//   * Request    — what does a VM need ? `ScheduleRequest.RequestedPCI`
//     is a slice of `PCIRequest{VendorID, DeviceID, Count}`. Match
//     semantics : the host's inventory must satisfy every entry —
//     a request for (vendor=8086, device=1572, count=2) only
//     places on a host carrying at least two such devices.
//
// **Exclusivity is NOT enforced today.** Two VMs requesting the
// same vendor:device pair will both schedule onto the host that
// carries it ; the operator's vfio binding script is what actually
// claims the BDF. The placement layer treats PCI as a cardinality
// filter, not a reservation — same model as `ScheduleRequest.GPU`
// (see docs/operations/pci-passthrough.md for the rationale and
// the BDF-stability gotcha).

import (
	"fmt"
	"strings"
)

// PCIDevice is one physical PCI(e) endpoint on the host that the
// operator has opted into for passthrough. Populated by the agent
// via detectPCI() (Linux : sysfs walk of /sys/bus/pci/devices/*) ;
// consumed by the scheduler when matching
// ScheduleRequest.RequestedPCI.
//
// Vendor / DeviceID are the 4-hex-digit PCI Vendor / Device codes
// (e.g. Intel 82599 = 8086:10fb) — lower-case canonical form, no
// "0x" prefix, leading zeros preserved. The driver field carries
// the Linux kernel driver currently bound to the device (typically
// "vfio-pci" for passthrough-ready endpoints, "nvme" / "i40e" /
// … for endpoints not yet unbound). Surfaced for operator
// visibility — the scheduler doesn't read it.
//
// BDF is the bus:device.function string in canonical 0000:bb:dd.f
// form (PCI domain always included, even on single-domain hosts).
// The driver layer passes BDF through to QEMU's `-device
// vfio-pci,host=<BDF>` flag verbatim.
type PCIDevice struct {
	BDF      string `json:"bdf"`                 // "0000:65:00.1"
	VendorID string `json:"vendor_id"`           // "8086" — PCI Code (lowercase hex, no 0x prefix)
	DeviceID string `json:"device_id"`           // "1572"
	Driver   string `json:"driver,omitempty"`    // kernel driver bound to the device (e.g. "vfio-pci")
}

// PCIRequest is one entry in ScheduleRequest.RequestedPCI. The
// matcher requires the host's inventory to carry at least Count
// devices whose (VendorID, DeviceID) tuple matches the request.
// Empty VendorID is a programming error — the scheduler-level
// validator surfaces an explicit error at the entry point ; the
// matcher returns false (mismatch) so callers stay defensive.
//
// Count defaults to 1 when zero — a request with `Count=0` would
// otherwise tautologically match every host, which is never what
// the operator meant.
type PCIRequest struct {
	VendorID string `json:"vendor_id"`
	DeviceID string `json:"device_id"`
	Count    int    `json:"count,omitempty"`
}

// pciBlock mirrors PCIDevice on the HCL side. The block label is
// the canonical BDF — BDFs are the only unique key inside one host
// (multiple ports of the same NIC share vendor:device but never
// the BDF), so we anchor the block label on it.
type pciBlock struct {
	BDF      string `hcl:",label"`
	VendorID string `hcl:"vendor_id,optional"`
	DeviceID string `hcl:"device_id,optional"`
	Driver   string `hcl:"driver,optional"`
}

// detectPCI is the agent-side populator for Host.PCIDevices.
//
// **Today this is a stub for non-Linux GOOS** (Linux body lives in
// pci_detect_linux.go). The Linux walk inspects
// /sys/bus/pci/devices/*/{vendor,device,driver} and reports back
// every endpoint — the agent then filters down to the ones the
// operator has explicitly bound to vfio-pci (so we don't surface
// the host's own NIC as "passthrough candidate" by accident).
//
// The signature is stable across the future swap : when the full
// Linux walk lands, this function keeps the same shape and every
// call site (host_self.go's selfRegisterHost) continues to work.
// Operators staging passthrough today seed inventory statically
// via cluster.hcl's `pci { … }` blocks instead.
func detectPCI() []PCIDevice {
	return detectPCIImpl()
}

// clonePCIDevices deep-copies a PCIDevice slice so the registry
// can't be mutated through the caller's spec pointer. Same pattern
// as cloneGPUs / cloneHostDrivers — nil/empty in → nil out so the
// JSON omitempty + HCL omit-when-empty contract stays clean.
func clonePCIDevices(src []PCIDevice) []PCIDevice {
	if len(src) == 0 {
		return nil
	}
	out := make([]PCIDevice, len(src))
	copy(out, src)
	return out
}

// pciRequestSatisfied reports whether a host's inventory satisfies
// one PCIRequest entry. Returns true when at least Count devices
// of matching VendorID + DeviceID exist on the host.
//
// Matching is case-insensitive on the hex IDs — operators write
// "8086:1572" / "8086:1572" / "8086:1572" interchangeably and the
// detector normalises to lowercase, but defensive comparison stays
// safe.
//
// Empty VendorID is a programming error ; the helper returns false
// (no match) so callers don't accidentally place "no PCI request"
// VMs onto every host. validatePCIRequests() in the scheduler
// surface rejects empty VendorID at the entry point so the false
// here is the defensive belt-and-braces.
func pciRequestSatisfied(req PCIRequest, devs []PCIDevice) bool {
	if req.VendorID == "" {
		return false
	}
	want := req.Count
	if want <= 0 {
		want = 1
	}
	matched := 0
	for _, d := range devs {
		if !strings.EqualFold(d.VendorID, req.VendorID) {
			continue
		}
		// DeviceID is optional on the request side : leaving it empty
		// means "any device from this vendor" (rare in practice — most
		// passthrough requests pin both — but useful for "any Intel
		// 82599-family NIC" style declarations once we ship a SKU
		// alias table). Same wildcard model GPURequest.Model uses.
		if req.DeviceID != "" && !strings.EqualFold(d.DeviceID, req.DeviceID) {
			continue
		}
		matched++
		if matched >= want {
			return true
		}
	}
	return false
}

// validatePCIRequests rejects malformed PCIRequest entries before
// they reach the scheduler. Empty VendorID is the only hard error ;
// missing DeviceID is the documented wildcard ; Count<=0 is
// normalised to 1 inside pciRequestSatisfied.
//
// Returns nil for an empty / nil slice — "no PCI request" is the
// common case (every non-passthrough VM) and must stay zero-cost.
func validatePCIRequests(reqs []PCIRequest) error {
	for i, r := range reqs {
		if r.VendorID == "" {
			return fmt.Errorf("pci request %d: vendor_id is required", i)
		}
	}
	return nil
}

// pciBDFsForRequests is the driver-side hand-off : given the
// chosen host's inventory + the VM's RequestedPCI, return the list
// of BDFs to attach. Greedy — walks the inventory in declared
// order and picks the first matching device per request slot.
// Idempotent ; deterministic across runs (inventory is stored
// sorted by detectPCI()).
//
// **No reservation accounting today.** Two concurrent calls with
// the same host + the same request will return overlapping BDF
// lists ; the operator's vfio binding is the actual claim. See
// docs/operations/pci-passthrough.md "we don't yet enforce
// exclusivity" for the rationale.
func pciBDFsForRequests(reqs []PCIRequest, devs []PCIDevice) []string {
	if len(reqs) == 0 {
		return nil
	}
	var out []string
	for _, r := range reqs {
		want := r.Count
		if want <= 0 {
			want = 1
		}
		for _, d := range devs {
			if !strings.EqualFold(d.VendorID, r.VendorID) {
				continue
			}
			if r.DeviceID != "" && !strings.EqualFold(d.DeviceID, r.DeviceID) {
				continue
			}
			out = append(out, d.BDF)
			want--
			if want == 0 {
				break
			}
		}
	}
	return out
}
