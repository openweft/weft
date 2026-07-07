package weft

import (
	"os"
	"path/filepath"
	"testing"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// TestPodSpecRegistry_SetGetEvict is the contract test for the in-
// memory pod_id → PodSpec store : the GuestPodPlane handler trusts
// these semantics when it builds the HelloAck.
func TestPodSpecRegistry_SetGetEvict(t *testing.T) {
	a := &Adapter{}
	a.initPodSpecs()

	if _, ok := a.PodSpec("missing"); ok {
		t.Errorf("PodSpec on empty registry should return (nil,false)")
	}

	want := &guestv1.PodSpec{
		Containers: []*guestv1.Container{{Id: "c1", RootfsTag: "r"}},
	}
	a.SetPodSpec("pod-a", want)
	got, ok := a.PodSpec("pod-a")
	if !ok || got == nil {
		t.Fatalf("PodSpec(pod-a) = (%v,%v), want (non-nil,true)", got, ok)
	}
	if got.PodId != "pod-a" {
		t.Errorf("SetPodSpec should stamp pod_id from the key ; got %q", got.PodId)
	}
	if len(got.Containers) != 1 || got.Containers[0].Id != "c1" {
		t.Errorf("PodSpec containers lost : %+v", got.Containers)
	}

	// nil spec evicts.
	a.SetPodSpec("pod-a", nil)
	if _, ok := a.PodSpec("pod-a"); ok {
		t.Errorf("SetPodSpec(_, nil) should evict the entry")
	}

	// Empty podID is silently ignored on Set — same convention as
	// the podCIDRegistry, protects against half-built VM records.
	a.SetPodSpec("", &guestv1.PodSpec{Containers: nil})
	if _, ok := a.PodSpec(""); ok {
		t.Errorf("SetPodSpec(\"\",_) should be a no-op")
	}
}

// TestPodSpecRegistry_NilAdapter pins the safe-on-zero-value path :
// the GuestPodPlane handler may be wired with an Adapter whose
// constructor didn't run (a misconfigured test), so PodSpec must
// return (nil,false) instead of panicking on the nil map.
func TestPodSpecRegistry_NilAdapter(t *testing.T) {
	var a Adapter // podSpecs == nil
	if _, ok := a.PodSpec("anything"); ok {
		t.Errorf("PodSpec on nil registry should return (nil,false)")
	}
	a.SetPodSpec("anything", &guestv1.PodSpec{}) // must not panic
}

// TestPodSpecRegistry_PersistAndReload verifies the HCL round-trip:
// SetPodSpec writes <stateDir>/podspecs.hcl atomically, and a fresh
// Adapter reading the same stateDir at startup rehydrates the spec
// without the operator having to re-publish. Covers the persistence
// contract that backs SetPodSpec + initPodSpecs.
func TestPodSpecRegistry_PersistAndReload(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{stateDir: dir}
	a.initPodSpecs()
	a.SetPodSpec("pod-x", &guestv1.PodSpec{
		Containers: []*guestv1.Container{{Id: "c1", RootfsTag: "rootfs0"}},
	})
	if _, err := os.Stat(filepath.Join(dir, "podspecs.hcl")); err != nil {
		t.Fatalf("podspecs.hcl was not persisted : %v", err)
	}
	// Fresh adapter pointing at the same stateDir should rehydrate.
	b := &Adapter{stateDir: dir}
	b.initPodSpecs()
	got, ok := b.PodSpec("pod-x")
	if !ok || got == nil {
		t.Fatalf("rehydrated PodSpec(pod-x) = (%v,%v), want (non-nil,true)", got, ok)
	}
	if got.PodId != "pod-x" {
		t.Errorf("rehydrated PodSpec.pod_id = %q, want pod-x", got.PodId)
	}
	if len(got.Containers) != 1 || got.Containers[0].Id != "c1" {
		t.Errorf("rehydrated containers lost : %+v", got.Containers)
	}
	// Eviction must also persist.
	b.SetPodSpec("pod-x", nil)
	c := &Adapter{stateDir: dir}
	c.initPodSpecs()
	if _, ok := c.PodSpec("pod-x"); ok {
		t.Errorf("after Set(nil) + reload, PodSpec(pod-x) should be evicted")
	}
}
