package weft

import "testing"

// TestAllocateVsockCID_Deterministic asserts the same inputs always
// produce the same CID — guarantees the GuestPodPlane peer check
// keeps working after an agent restart, even before initPodCIDs has
// rehydrated from disk.
func TestAllocateVsockCID_Deterministic(t *testing.T) {
	const proj = "proj-uuid-1"
	const vm = "vm-uuid-1"
	first := AllocateVsockCID(proj, vm)
	for i := 0; i < 10; i++ {
		if got := AllocateVsockCID(proj, vm); got != first {
			t.Fatalf("AllocateVsockCID drift on iter %d : %d vs %d", i, got, first)
		}
	}
}

// TestAllocateVsockCID_InRange asserts every allocation lands in the
// usable window [VsockCIDFirst, VsockCIDLast] — never the reserved
// 0/1/2 or 0xffffffff values that would alias the kernel's special
// CIDs.
func TestAllocateVsockCID_InRange(t *testing.T) {
	// A handful of well-distributed inputs ; the hash spreads values
	// enough that a small sample is representative.
	cases := []struct{ proj, vm string }{
		{"a", "b"},
		{"proj-1", "vm-1"},
		{"proj-1", "vm-2"},
		{"", "lone-vm"},
		{"proj-2", "vm-zzzzzzzzzzzzzzzzzzzzzzzz"},
	}
	for _, c := range cases {
		cid := AllocateVsockCID(c.proj, c.vm)
		if cid < VsockCIDFirst || cid > VsockCIDLast {
			t.Errorf("AllocateVsockCID(%q,%q) = %d, want in [%d,%d]",
				c.proj, c.vm, cid, VsockCIDFirst, VsockCIDLast)
		}
		if cid == 0 || cid == 1 || cid == 2 || cid == 0xffffffff {
			t.Errorf("AllocateVsockCID(%q,%q) = %d, hit a reserved CID",
				c.proj, c.vm, cid)
		}
	}
}

// TestAllocateVsockCID_EmptyVMReturnsZero documents the legacy fall-
// back : callers that don't have a VM UUID get 0 (= unassigned), so
// the Adapter knows to skip persisting + the GuestPodPlane treats
// the pod as "unknown, no strict expectation".
func TestAllocateVsockCID_EmptyVMReturnsZero(t *testing.T) {
	if got := AllocateVsockCID("anything", ""); got != 0 {
		t.Errorf("AllocateVsockCID(_, \"\") = %d, want 0", got)
	}
}

// TestPodCIDRegistry_SetGetDelete is the contract test for the in-
// memory pod_id→CID index.
func TestPodCIDRegistry_SetGetDelete(t *testing.T) {
	r := newPodCIDRegistry()

	if _, ok := r.Get("missing"); ok {
		t.Errorf("Get on empty registry should return (0,false)")
	}

	r.Set("pod-a", 42)
	r.Set("pod-b", 100)
	if got, ok := r.Get("pod-a"); !ok || got != 42 {
		t.Errorf("Get(pod-a) = (%d,%v), want (42,true)", got, ok)
	}
	if got, ok := r.Get("pod-b"); !ok || got != 100 {
		t.Errorf("Get(pod-b) = (%d,%v), want (100,true)", got, ok)
	}

	// Setting cid=0 evicts (allocator never returns 0 for valid input).
	r.Set("pod-a", 0)
	if _, ok := r.Get("pod-a"); ok {
		t.Errorf("Set(_,0) should evict the entry")
	}

	r.Delete("pod-b")
	if _, ok := r.Get("pod-b"); ok {
		t.Errorf("Delete should drop the entry")
	}

	// Delete on missing is a no-op (no panic, no error).
	r.Delete("never-set")

	// Empty pod_id is silently ignored on Set — protects against
	// callers that pass a half-built VM record.
	r.Set("", 1)
	if _, ok := r.Get(""); ok {
		t.Errorf("Set(\"\",_) should be a no-op")
	}
}
