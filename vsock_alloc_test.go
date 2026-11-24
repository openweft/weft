package weft

import (
	"context"
	"strconv"
	"testing"
)

func kvAdapter() (*Adapter, *MemKVStorage) {
	kv := NewMemKVStorage("/weft/vsockcids/")
	return &Adapter{kvStorageFactory: func(string) KVStorage { return kv }}, kv
}

func ckey(c uint32) string { return strconv.FormatUint(uint64(c), 10) }

// First on purpose: a missing nil-registry guard must show up as an
// assertion before any later test can take the binary down.
func TestInventoryHolds_NilRegistry(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("inventoryHolds panicked without a registry: %v", r)
		}
	}()
	if (&Adapter{}).inventoryHolds(70000, "p/vm") {
		t.Fatal("held with no inventory")
	}
}

func TestClaimVsockCID_KVCollisionProbesForward(t *testing.T) {
	a, kv := kvAdapter()
	start := AllocateVsockCID("p", "vm")
	// Another VM (maybe on another host) already owns the hash slot and the next one.
	ctx := context.Background()
	_ = kv.PutOne(ctx, ckey(start), []byte("q/other"))
	_ = kv.PutOne(ctx, ckey(start+1), []byte("q/other2"))
	got := a.claimVsockCID("p", "vm")
	if got != start+2 {
		t.Fatalf("claim = %d, want %d", got, start+2)
	}
	owner, _ := kv.GetOne(ctx, ckey(got))
	if string(owner) != "p/vm" {
		t.Fatalf("owner = %q", owner)
	}
}

func TestClaimVsockCID_KVStableForSameVM(t *testing.T) {
	a, _ := kvAdapter()
	first := a.claimVsockCID("p", "vm")
	if first == 0 {
		t.Fatal("zero CID")
	}
	if again := a.claimVsockCID("p", "vm"); again != first {
		t.Fatalf("second claim %d != first %d", again, first)
	}
}

func TestClaimVsockCID_EmptyName(t *testing.T) {
	a, _ := kvAdapter()
	if got := a.claimVsockCID("p", ""); got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestClaimVsockCID_KVReuseAfterRelease(t *testing.T) {
	a, kv := kvAdapter()
	ctx := context.Background()
	start := AllocateVsockCID("p", "vm")
	_ = kv.PutOne(ctx, ckey(start), []byte("q/other"))
	got := a.claimVsockCID("p", "vm")
	if got == start {
		t.Fatal("took a CID owned by another VM")
	}
	// A release by a non-owner must not free someone else's claim.
	a.releaseVsockCID("p", "vm", start)
	if v, _ := kv.GetOne(ctx, ckey(start)); string(v) != "q/other" {
		t.Fatalf("non-owner release removed the claim: %q", v)
	}
	// The other VM exits: its CID is reusable by a newcomer hashing there.
	a.releaseVsockCID("q", "other", start)
	if v, _ := kv.GetOne(ctx, ckey(start)); v != nil {
		t.Fatalf("claim survived release: %q", v)
	}
	// Own release frees the slot for reuse.
	a.releaseVsockCID("p", "vm", got)
	if v, _ := kv.GetOne(ctx, ckey(got)); v != nil {
		t.Fatalf("own claim survived release: %q", v)
	}
	if re := a.claimVsockCID("p", "vm"); re != start {
		t.Fatalf("after release the hash slot is free again: got %d want %d", re, start)
	}
}

func TestClaimVsockCID_LocalFallback(t *testing.T) {
	a := &Adapter{}
	start := AllocateVsockCID("p", "vm")
	a.vsockLocal = map[uint32]string{start: "q/other"}
	// A bare adapter has neither inventory nor store: a missing nil
	// guard must fail as an assertion, not kill the test binary.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked on an adapter without inventory/store: %v", r)
		}
	}()
	got := a.claimVsockCID("p", "vm")
	if got != start+1 {
		t.Fatalf("claim = %d, want %d", got, start+1)
	}
	if again := a.claimVsockCID("p", "vm"); again != got {
		t.Fatalf("not stable: %d vs %d", again, got)
	}
	a.releaseVsockCID("p", "vm", got)
	if _, taken := a.vsockLocal[got]; taken {
		t.Fatal("local claim survived release")
	}
	a.releaseVsockCID("p", "vm", start) // not the owner of start
	if a.vsockLocal[start] != "q/other" {
		t.Fatal("non-owner release removed a local claim")
	}
}

// errKV injects store failures on top of a MemKVStorage.
type errKV struct {
	*MemKVStorage
	createErr, getErr error
	creates, gets     int
}

func (e *errKV) CreateOne(ctx context.Context, k string, b []byte) (bool, error) {
	e.creates++
	if e.createErr != nil {
		return false, e.createErr
	}
	return e.MemKVStorage.CreateOne(ctx, k, b)
}

func (e *errKV) GetOne(ctx context.Context, k string) ([]byte, error) {
	e.gets++
	if e.getErr != nil {
		return nil, e.getErr
	}
	return e.MemKVStorage.GetOne(ctx, k)
}

func TestClaimVsockCID_StoreErrorsFallBackToLocal(t *testing.T) {
	start := AllocateVsockCID("p", "vm")
	t.Run("create fails", func(t *testing.T) {
		kv := &errKV{MemKVStorage: NewMemKVStorage("x"), createErr: context.DeadlineExceeded}
		a := &Adapter{kvStorageFactory: func(string) KVStorage { return kv }}
		if got := a.claimVsockCID("p", "vm"); got != start {
			t.Fatalf("got %d, want %d", got, start)
		}
		if kv.creates != 1 {
			t.Fatalf("%d create attempts after a store error, want 1 (give up at once)", kv.creates)
		}
		if a.vsockLocal[start] != "p/vm" {
			t.Fatal("not claimed locally after a store error")
		}
	})
	t.Run("get fails after a lost race", func(t *testing.T) {
		mem := NewMemKVStorage("x")
		_ = mem.PutOne(context.Background(), ckey(start), []byte("q/other"))
		kv := &errKV{MemKVStorage: mem, getErr: context.DeadlineExceeded}
		a := &Adapter{kvStorageFactory: func(string) KVStorage { return kv }}
		got := a.claimVsockCID("p", "vm")
		if got == 0 || a.vsockLocal[got] != "p/vm" {
			t.Fatalf("got %d, local = %v", got, a.vsockLocal)
		}
	})
}

func TestClaimVsockCID_EmptyNameClaimsNothing(t *testing.T) {
	a, kv := kvAdapter()
	if got := a.claimVsockCID("p", ""); got != 0 {
		t.Fatalf("got %d", got)
	}
	if m, _ := kv.List(context.Background()); len(m) != 0 {
		t.Fatalf("claimed %v for an empty name", m)
	}
	if len(a.vsockLocal) != 0 {
		t.Fatalf("local claims %v", a.vsockLocal)
	}
}

func TestClaimVsockCID_CleanClaimIsOneCreate(t *testing.T) {
	kv := &errKV{MemKVStorage: NewMemKVStorage("x")}
	a := &Adapter{kvStorageFactory: func(string) KVStorage { return kv }}
	if got := a.claimVsockCID("p", "vm"); got != AllocateVsockCID("p", "vm") {
		t.Fatalf("got %d", got)
	}
	if kv.creates != 1 || kv.gets != 0 {
		t.Fatalf("creates=%d gets=%d, want 1 and 0: a won create returns at once", kv.creates, kv.gets)
	}
}

func TestClaimVsockCID_KVHoldsTheClaim(t *testing.T) {
	a, kv := kvAdapter()
	got := a.claimVsockCID("p", "vm")
	m, _ := kv.List(context.Background())
	if len(m) != 1 || string(m[ckey(got)]) != "p/vm" {
		t.Fatalf("store = %v, want only %d=p/vm", m, got)
	}
}

func TestReleaseVsockCID_Guards(t *testing.T) {
	ctx := context.Background()
	t.Run("zero cid", func(t *testing.T) {
		a, kv := kvAdapter()
		_ = kv.PutOne(ctx, "0", []byte("p/vm"))
		a.releaseVsockCID("p", "vm", 0)
		if v, _ := kv.GetOne(ctx, "0"); v == nil {
			t.Fatal("released CID 0")
		}
	})
	t.Run("empty name", func(t *testing.T) {
		a, kv := kvAdapter()
		_ = kv.PutOne(ctx, "70000", []byte("p/"))
		a.releaseVsockCID("p", "", 70000)
		if v, _ := kv.GetOne(ctx, "70000"); v == nil {
			t.Fatal("released a claim for an empty name")
		}
	})
	t.Run("store without create-if-absent is left alone", func(t *testing.T) {
		mem := NewMemKVStorage("x")
		_ = mem.PutOne(ctx, "70000", []byte("p/vm"))
		var plain KVStorage = struct{ KVStorage }{mem} // hides CreateOne
		a := &Adapter{kvStorageFactory: func(string) KVStorage { return plain }}
		a.releaseVsockCID("p", "vm", 70000)
		if v, _ := mem.GetOne(ctx, "70000"); v == nil {
			t.Fatal("deleted from a store that does not own the claims")
		}
	})
}

func TestClaimVsockCID_DistinctVMsNeverShare(t *testing.T) {
	a, _ := kvAdapter()
	seen := map[uint32]string{}
	for i := 0; i < 200; i++ {
		name := "vm-" + strconv.Itoa(i)
		c := a.claimVsockCID("p", name)
		if !IsVsockGuestCID(c) {
			t.Fatalf("%s: bad CID %d", name, c)
		}
		if prev, dup := seen[c]; dup {
			t.Fatalf("%s and %s share CID %d", name, prev, c)
		}
		seen[c] = name
	}
}

func inventoryAdapter(t *testing.T, a *Adapter, vms ...VM) {
	t.Helper()
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	for _, v := range vms {
		made, err := reg.create(CreateVMSpec{ProjectUUID: v.ProjectUUID, Name: v.Name, HostUUID: "h1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.setVsockCID(made.UUID, v.VsockCID); err != nil {
			t.Fatal(err)
		}
	}
	a.vmReg = reg
}

func TestClaimVsockCID_InventoryHolderIsSkipped(t *testing.T) {
	a, _ := kvAdapter()
	start := AllocateVsockCID("p", "vm")
	// A VM registered before claims existed holds the hash slot.
	inventoryAdapter(t, a, VM{ProjectUUID: "q", Name: "old", VsockCID: start})
	if got := a.claimVsockCID("p", "vm"); got != start+1 {
		t.Fatalf("claim = %d, want %d", got, start+1)
	}
}

func TestClaimVsockCID_VMKeepsItsRecordedCID(t *testing.T) {
	a, _ := kvAdapter()
	start := AllocateVsockCID("p", "vm")
	inventoryAdapter(t, a, VM{ProjectUUID: "p", Name: "vm", VsockCID: start + 7})
	if got := a.claimVsockCID("p", "vm"); got != start+7 {
		t.Fatalf("claim = %d, want its recorded %d", got, start+7)
	}
}

func TestNextCIDWraps(t *testing.T) {
	if got := nextCID(VsockCIDLast); got != VsockCIDFirst {
		t.Fatalf("wrap = %d", got)
	}
	if got := nextCID(VsockCIDFirst); got != VsockCIDFirst+1 {
		t.Fatalf("next = %d", got)
	}
}

func TestMemKVCreateOne(t *testing.T) {
	kv := NewMemKVStorage("x")
	ctx := context.Background()
	if ok, _ := kv.CreateOne(ctx, "k", []byte("a")); !ok {
		t.Fatal("first create refused")
	}
	if ok, _ := kv.CreateOne(ctx, "k", []byte("b")); ok {
		t.Fatal("second create succeeded")
	}
	if v, _ := kv.GetOne(ctx, "k"); string(v) != "a" {
		t.Fatalf("value = %q", v)
	}
}
