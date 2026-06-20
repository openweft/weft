package weft

// pod_specs.go owns the in-memory store of GuestPodPlane PodSpecs.
// A PodSpec is the operator's desired-state description of what
// containers, mounts, restart policy, etc. should be running inside
// a microVM. The host serves it to the guest on GuestPodPlane.Attach
// via the HelloAck frame ; weft-microvm-agent's containers
// subscriber reconciles toward it.
//
// Today the store is purely in-memory : nothing forces a restart
// to lose the operator's published specs, but reseeding after a
// crash is the operator's responsibility (re-issue the SetPodSpec
// RPC, which a follow-up commit will add to the agent surface).
// HCL persistence + an etcd-watch-driven hot reload land alongside
// that RPC ; the in-memory layer below is the contract the rest
// of the code consumes either way.

import (
	"sync"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// podSpecRegistry is the pod_id → *guestv1.PodSpec map the
// GuestPodPlane handler reads at Hello time. Goroutine-safe ; the
// only mutator is SetPodSpec, the only reader is PodSpec. Both
// surface via the Adapter so callers don't peek through the lock.
type podSpecRegistry struct {
	mu sync.RWMutex
	m  map[string]*guestv1.PodSpec
}

func newPodSpecRegistry() *podSpecRegistry {
	return &podSpecRegistry{m: make(map[string]*guestv1.PodSpec)}
}

// initPodSpecs is the lifecycle hook the Adapter constructor calls.
// Symmetric with initPodCIDs : no-op today (no persistence) ; once
// HCL backing lands the same call site rehydrates from disk.
func (a *Adapter) initPodSpecs() {
	a.podSpecs = newPodSpecRegistry()
}

// SetPodSpec records the operator's desired pod state for the
// given pod_id (= VM.Name on the wire contract). Passing nil
// evicts the entry — gives the operator a clean "drop the spec
// and let the guest stay attached without one" path.
func (a *Adapter) SetPodSpec(podID string, spec *guestv1.PodSpec) {
	if a.podSpecs == nil || podID == "" {
		return
	}
	a.podSpecs.mu.Lock()
	defer a.podSpecs.mu.Unlock()
	if spec == nil {
		delete(a.podSpecs.m, podID)
		return
	}
	// Defensive copy of the pod_id field so a caller mutating spec
	// after SetPodSpec doesn't drift the on-wire representation.
	// The rest of the spec is value-passed to the guest via gRPC
	// so internal mutation doesn't leak — keep this minimal.
	spec.PodId = podID
	a.podSpecs.m[podID] = spec
}

// PodSpec returns the operator-supplied desired state for the pod
// plus a bool indicating whether one has been published yet.
// Unknown pods → (nil, false), which the GuestPodPlane handler
// surfaces as an empty PodSpec on HelloAck so the guest still
// receives a valid ack.
func (a *Adapter) PodSpec(podID string) (*guestv1.PodSpec, bool) {
	if a.podSpecs == nil {
		return nil, false
	}
	a.podSpecs.mu.RLock()
	defer a.podSpecs.mu.RUnlock()
	s, ok := a.podSpecs.m[podID]
	return s, ok
}
