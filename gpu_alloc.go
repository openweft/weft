package weft

// gpu_alloc.go is the counted-allocation layer that turns the GPU
// scheduler's *filter* into an *exclusive claim*. Without it, two VMs
// with the same GPU request both schedule onto one card and collide at
// VFIO bind time (the "Exclusivity boundary" gap in
// docs/operations/gpu-scheduling.md). With it, a card or a MIG instance
// is held by at most one VM until that VM is deprovisioned.
//
// Scope of THIS file : the in-memory primitive + the exclusivity-aware
// host matcher. It is deliberately not yet wired into ScheduleVM /
// DeprovisionVM, and it does not yet persist to etcd — both are tracked
// follow-ups (see docs/operations/gpu-sharing.md, "Phased delivery").
// Keeping the primitive standalone and fully tested first mirrors how
// the GPU axis itself landed : inventory + matching before allocation.
//
// Allocatable resource → resource id :
//
//	whole card    → GPU.PCIBDF        (attached via vfio-pci,host=<BDF>)
//	MIG instance  → MIGInstance.UUID  (attached via vfio-pci,sysfsdev=<uuid>)
//
// A card reports EITHER a whole-card resource OR its MIG instances
// (never both — see GPU.MIGInstances), so a whole-card claim and a MIG
// claim on the same physical card can't coexist by construction.

import (
	"fmt"
	"sync"
)

// GPUClaimKind distinguishes a whole-card claim from a MIG-slice claim.
// It travels with the claim so the release / audit paths and the future
// etcd record can tell the two resource id namespaces apart (a BDF and a
// MIG UUID never collide in practice, but the kind keeps intent explicit).
type GPUClaimKind string

const (
	// GPUClaimWholeCard holds an entire physical GPU, keyed by PCI BDF.
	GPUClaimWholeCard GPUClaimKind = "card"
	// GPUClaimMIG holds one MIG instance, keyed by its MIG / mdev UUID.
	GPUClaimMIG GPUClaimKind = "mig"
)

// GPUClaim records that one allocatable GPU resource on a host is held
// by a VM. ResourceID is the PCI BDF (whole card) or the MIG-instance
// UUID (MIG). Claims are exclusive : a (HostUUID, ResourceID) pair holds
// at most one live claim.
//
// CreatedAtUnixNs is stamped by the caller (the table never reads the
// clock itself, so it stays deterministic for tests). Model is carried
// for diagnostics / "what's holding my H200?" introspection.
type GPUClaim struct {
	HostUUID        string       `json:"host_uuid"`
	ResourceID      string       `json:"resource_id"`
	Kind            GPUClaimKind `json:"kind"`
	VMUUID          string       `json:"vm_uuid"`
	Model           string       `json:"model,omitempty"`
	CreatedAtUnixNs int64        `json:"created_at_unix_ns,omitempty"`
}

// claimKey is the table's internal map key — host-scoped so the same BDF
// on two different hosts is two distinct resources.
func claimKey(hostUUID, resourceID string) string {
	return hostUUID + "/" + resourceID
}

// gpuAllocTable is an in-memory, exclusive claim table. Safe for
// concurrent use. The future etcd-backed store (/weft/gpu/allocations/*,
// mirroring weft-network's /weft/network/*) will implement the same
// Claim / Release surface so call sites don't change when persistence
// lands.
type gpuAllocTable struct {
	mu         sync.Mutex
	byResource map[string]GPUClaim // claimKey(host, resource) → claim
	byVM       map[string][]string // vm uuid → claimKeys it holds (for ReleaseVM)
}

// newGPUAllocTable returns an empty claim table.
func newGPUAllocTable() *gpuAllocTable {
	return &gpuAllocTable{
		byResource: make(map[string]GPUClaim),
		byVM:       make(map[string][]string),
	}
}

// Claim records an exclusive hold on (HostUUID, ResourceID).
//
// Idempotent for the SAME VM : re-claiming a resource the VM already
// holds updates the record and returns nil (so a scheduler retry after a
// transient failure doesn't error). Returns an error when the resource
// is already held by a DIFFERENT VM — that's the collision the layer
// exists to prevent.
//
// Validation : HostUUID, ResourceID and VMUUID are all required.
func (t *gpuAllocTable) Claim(c GPUClaim) error {
	if c.HostUUID == "" || c.ResourceID == "" || c.VMUUID == "" {
		return fmt.Errorf("gpu claim: host_uuid, resource_id and vm_uuid are all required (got host=%q resource=%q vm=%q)",
			c.HostUUID, c.ResourceID, c.VMUUID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := claimKey(c.HostUUID, c.ResourceID)
	if existing, ok := t.byResource[key]; ok {
		if existing.VMUUID != c.VMUUID {
			return fmt.Errorf("gpu claim: resource %s on host %s already held by vm %s",
				c.ResourceID, c.HostUUID, existing.VMUUID)
		}
		// Same VM re-claiming : refresh the record in place, don't
		// double-append to byVM.
		t.byResource[key] = c
		return nil
	}
	t.byResource[key] = c
	t.byVM[c.VMUUID] = append(t.byVM[c.VMUUID], key)
	return nil
}

// Release drops the claim on (HostUUID, ResourceID), if any. Idempotent :
// releasing an unheld resource is a no-op (mirrors the driver Delete*
// "already gone → nil" contract). Returns true when a claim was actually
// removed.
func (t *gpuAllocTable) Release(hostUUID, resourceID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.releaseLocked(claimKey(hostUUID, resourceID))
}

// ReleaseVM drops every claim held by a VM — the DeprovisionVM path, so a
// deleted VM never leaks a held GPU. Returns the number of claims freed.
func (t *gpuAllocTable) ReleaseVM(vmUUID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := t.byVM[vmUUID]
	freed := 0
	for _, key := range keys {
		if t.releaseResourceOnlyLocked(key) {
			freed++
		}
	}
	delete(t.byVM, vmUUID)
	return freed
}

// releaseLocked removes a single resource claim and prunes the owning
// VM's byVM entry. Caller holds t.mu.
func (t *gpuAllocTable) releaseLocked(key string) bool {
	claim, ok := t.byResource[key]
	if !ok {
		return false
	}
	delete(t.byResource, key)
	t.pruneVMKeyLocked(claim.VMUUID, key)
	return true
}

// releaseResourceOnlyLocked removes the byResource entry without touching
// byVM — used by ReleaseVM, which clears the whole byVM slice in one go.
func (t *gpuAllocTable) releaseResourceOnlyLocked(key string) bool {
	if _, ok := t.byResource[key]; !ok {
		return false
	}
	delete(t.byResource, key)
	return true
}

// pruneVMKeyLocked removes one claimKey from a VM's byVM slice, deleting
// the map entry when the VM holds nothing more. Caller holds t.mu.
func (t *gpuAllocTable) pruneVMKeyLocked(vmUUID, key string) {
	keys := t.byVM[vmUUID]
	out := keys[:0]
	for _, k := range keys {
		if k != key {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		delete(t.byVM, vmUUID)
		return
	}
	t.byVM[vmUUID] = out
}

// IsClaimed reports whether (HostUUID, ResourceID) is currently held.
func (t *gpuAllocTable) IsClaimed(hostUUID, resourceID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.byResource[claimKey(hostUUID, resourceID)]
	return ok
}

// ClaimsForHost returns a copy of every live claim on a host. Order is
// unspecified — callers that need stable output sort it themselves.
func (t *gpuAllocTable) ClaimsForHost(hostUUID string) []GPUClaim {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []GPUClaim
	for _, c := range t.byResource {
		if c.HostUUID == hostUUID {
			out = append(out, c)
		}
	}
	return out
}

// hostClaimChecker returns a `claimed(resourceID) bool` closure bound to
// one host, suitable for passing to gpuRequestSatisfiedExcl. Snapshots
// the host's held resource ids under the lock so the matcher sees a
// consistent view without holding t.mu across the (caller's) loop.
func (t *gpuAllocTable) hostClaimChecker(hostUUID string) func(resourceID string) bool {
	t.mu.Lock()
	held := make(map[string]struct{})
	for _, c := range t.byResource {
		if c.HostUUID == hostUUID {
			held[c.ResourceID] = struct{}{}
		}
	}
	t.mu.Unlock()
	return func(resourceID string) bool {
		_, ok := held[resourceID]
		return ok
	}
}

// HostSatisfiesExcl reports whether a host can satisfy EVERY entry in a
// per-VM GPU request under exclusivity — the claim-aware analogue of the
// scheduler's RequestedGPUs loop. Used by the (follow-up) scheduler
// wiring; exported on the table so that integration lands as a one-line
// call swap rather than re-threading the claim view through hostMatches.
func (t *gpuAllocTable) HostSatisfiesExcl(reqs []GPURequest, h Host) bool {
	if len(reqs) == 0 {
		return true
	}
	claimed := t.hostClaimChecker(h.UUID)
	for _, r := range reqs {
		if !gpuRequestSatisfiedExcl(r, h.GPUs, claimed) {
			return false
		}
	}
	return true
}
