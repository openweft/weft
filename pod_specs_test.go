package weft

import (
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
