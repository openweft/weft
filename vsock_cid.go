package weft

// vsock_cid.go owns the AF_VSOCK CID assignment policy for guest
// microVMs. The kernel reserves CIDs 0/1/2 + 0xffffffff for special
// addresses ; real guests live in [3, 2^32-2). We carve our own
// stable range derived from each VM's UUID so :
//
//   * Two VMs on the same host never collide (UUIDs are unique per
//     project ; we hash the canonical "project_uuid/vm_uuid" string).
//   * The CID is reproducible across agent restarts — the registry
//     re-computes from the VM record without needing a persisted
//     counter.
//   * The range stays away from CIDs other software might claim
//     (loopback experiments, libvirt's default pool starting at 3).
//
// The chosen window is [VsockCIDFirst, VsockCIDLast] :
//   VsockCIDFirst = 0x00010000  (65536 = first non-reserved range
//                                after libvirt's typical 3..255)
//   VsockCIDLast  = 0xfffefffe  (avoid 0xffffffff = CID_ANY)
//
// Collisions across hosts in the same cluster are fine — vsock is
// scoped per-hypervisor, not cluster-wide. The peer-CID check in
// GuestPodPlane.Attach reads the LOCAL agent's registry, so any
// other host's allocations are invisible.

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
)

const (
	// VsockCIDFirst is the lowest CID this allocator will hand out.
	// Picked above the typical 3..255 range hypervisors hand-pick
	// for early-boot use cases (libvirt's default machine vsock pool).
	VsockCIDFirst uint32 = 0x00010000
	// VsockCIDLast is one below the reserved CID_ANY (0xffffffff).
	VsockCIDLast uint32 = 0xfffefffe
	// vsockCIDRange is the inclusive size of the allocation window.
	vsockCIDRange uint32 = VsockCIDLast - VsockCIDFirst + 1
)

// AllocateVsockCID derives a deterministic guest CID from the given
// (projectUUID, vmIdentifier) pair. Same input → same output, same
// agent restart or different. Empty identifier returns 0 (unassigned)
// so the caller can fall back to legacy "no vsock" behaviour.
//
// vmIdentifier must be a value that uniquely names the VM within the
// project AND is available at every site that needs the CID :
//   - weft writes the CID into <vmDir>/config.json BEFORE
//     RegisterVM mints VM.UUID, so it has only `name` at that point.
//   - The agent's podCIDs index keys on pod_id (= VM.Name in the
//     GuestPodPlane wire contract).
//   - The persistent VM record stores the CID on VM.VsockCID.
//
// All three sites use the VM name. The name is unique within its
// project (vmRegistry refuses duplicates), so it's a safe identity
// key — and using the name everywhere keeps the three persistence
// sites in lockstep without threading the UUID through a layer that
// doesn't have it yet.
func AllocateVsockCID(projectUUID, vmIdentifier string) uint32 {
	if vmIdentifier == "" {
		return 0
	}
	key := projectUUID + "/" + vmIdentifier
	h := sha256.Sum256([]byte(key))
	// Take the first 4 bytes of the hash, fold into the window.
	raw := binary.BigEndian.Uint32(h[:4])
	return VsockCIDFirst + (raw % vsockCIDRange)
}

// podCIDRegistry caches the pod_id → CID mapping for in-process
// lookups by GuestPodPlane.Attach. Persistence lives on the VM
// record (VM.VsockCID) ; this is just a hot-path index so we don't
// scan the inventory on every Attach.
type podCIDRegistry struct {
	mu sync.RWMutex
	m  map[string]uint32 // pod_id (VM.Name) → CID
}

func newPodCIDRegistry() *podCIDRegistry {
	return &podCIDRegistry{m: make(map[string]uint32)}
}

// Set records the CID for a pod. Idempotent ; setting to 0 evicts.
func (r *podCIDRegistry) Set(podID string, cid uint32) {
	if podID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cid == 0 {
		delete(r.m, podID)
		return
	}
	r.m[podID] = cid
}

// Get returns the recorded CID + true if the pod is known. (0, false)
// for unknown pods — callers may treat that as "no expectation yet".
func (r *podCIDRegistry) Get(podID string) (uint32, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.m[podID]
	return c, ok
}

// Delete drops the entry. Safe on missing keys.
func (r *podCIDRegistry) Delete(podID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, podID)
}

// initPodCIDs builds a fresh, empty in-memory registry. v0.4.51 :
// the registry is now strict runtime truth — populated by
// GuestPodPlane.Attach's autoregister-on-first-Hello path using
// peer.CID() from the kernel. No more rehydration from VM
// inventory, which previously baked the allocator's pick into
// the registry and (silently) rejected Apple-VZ guests whose
// kernel-assigned CID differed from the allocator's hash. After
// an agent restart there's a brief window where strict-when-known
// is dormant for each pod, until the guest's first Hello self-
// announces ; that's the documented tradeoff.
func (a *Adapter) initPodCIDs() {
	a.podCIDs = newPodCIDRegistry()
}

// PodCID returns the registered AF_VSOCK CID for a pod_id (the VM
// Name carried in GuestFrame.Hello.pod_id) plus a bool indicating
// whether the registry knows about it. Unknown pods → (0, false) ;
// the caller (Attach) interprets that as "no strict expectation
// yet, fall through to the non-reserved CID guard".
func (a *Adapter) PodCID(podID string) (uint32, bool) {
	if a.podCIDs == nil {
		return 0, false
	}
	return a.podCIDs.Get(podID)
}

// RegisterPodCID associates a pod_id with its allocated CID at
// VM-registration time. Called by RegisterMicroVM after the VM
// inventory entry has been created. Idempotent.
func (a *Adapter) RegisterPodCID(podID string, cid uint32) {
	if a.podCIDs == nil || podID == "" || cid == 0 {
		return
	}
	a.podCIDs.Set(podID, cid)
}

// UnregisterPodCID drops the in-memory binding. Called by
// UnregisterVM so a CID that gets recycled (same VM UUID reused
// after a delete + register) doesn't leak across registrations.
func (a *Adapter) UnregisterPodCID(podID string) {
	if a.podCIDs == nil {
		return
	}
	a.podCIDs.Delete(podID)
}
