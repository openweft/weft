package weft

import (
	"context"
	"strings"
	"testing"
)

func newTestVMPropRegistry(t *testing.T) *vmPropertyRegistry {
	t.Helper()
	reg, err := LoadVMPropertyRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("LoadVMPropertyRegistry: %v", err)
	}
	return reg
}

func TestVMPropertyRegistry_FreshEmpty(t *testing.T) {
	reg := newTestVMPropRegistry(t)
	if got := reg.ListForVM("p", "vm-1"); len(got) != 0 {
		t.Errorf("fresh registry should return empty, got %d", len(got))
	}
}

func TestVMPropertyRegistry_SetListRoundtrip(t *testing.T) {
	reg := newTestVMPropRegistry(t)
	for _, p := range []VMProperty{
		{VMName: "web-1", Project: "alpha", Key: "owner", Value: "team-a", GuestReadable: true},
		{VMName: "web-1", Project: "alpha", Key: "cost-center", Value: "AB-1234"},
		{VMName: "web-1", Project: "alpha", Key: "tier", Value: "production", GuestReadable: true},
	} {
		if err := reg.Set(p); err != nil {
			t.Fatal(err)
		}
	}
	got := reg.ListForVM("alpha", "web-1")
	if len(got) != 3 {
		t.Fatalf("expected 3 props, got %d", len(got))
	}
	// Sorted by key.
	wantOrder := []string{"cost-center", "owner", "tier"}
	for i, w := range wantOrder {
		if got[i].Key != w {
			t.Errorf("order mismatch at %d : got %s, want %s", i, got[i].Key, w)
		}
	}
	// UpdatedAt stamped.
	for _, p := range got {
		if p.UpdatedAt.IsZero() {
			t.Errorf("UpdatedAt zero for %s", p.Key)
		}
	}
}

func TestVMPropertyRegistry_ProjectScopes(t *testing.T) {
	reg := newTestVMPropRegistry(t)
	_ = reg.Set(VMProperty{VMName: "vm", Project: "a", Key: "k", Value: "v-a"})
	_ = reg.Set(VMProperty{VMName: "vm", Project: "b", Key: "k", Value: "v-b"})
	// Same VM name in two projects must NOT collide.
	a := reg.ListForVM("a", "vm")
	b := reg.ListForVM("b", "vm")
	if len(a) != 1 || a[0].Value != "v-a" {
		t.Errorf("project a leak : %+v", a)
	}
	if len(b) != 1 || b[0].Value != "v-b" {
		t.Errorf("project b leak : %+v", b)
	}
}

func TestVMPropertyRegistry_SetReplaces(t *testing.T) {
	reg := newTestVMPropRegistry(t)
	_ = reg.Set(VMProperty{VMName: "vm", Project: "p", Key: "k", Value: "v1"})
	_ = reg.Set(VMProperty{VMName: "vm", Project: "p", Key: "k", Value: "v2", GuestReadable: true})
	got := reg.ListForVM("p", "vm")
	if len(got) != 1 || got[0].Value != "v2" || !got[0].GuestReadable {
		t.Errorf("Set should replace : %+v", got)
	}
}

func TestVMPropertyRegistry_DeleteIdempotent(t *testing.T) {
	reg := newTestVMPropRegistry(t)
	_ = reg.Set(VMProperty{VMName: "vm", Project: "p", Key: "k", Value: "v"})
	if err := reg.Delete("p", "vm", "k"); err != nil {
		t.Fatal(err)
	}
	if got := reg.ListForVM("p", "vm"); len(got) != 0 {
		t.Errorf("delete did not remove : %+v", got)
	}
	// Second delete : nil error.
	if err := reg.Delete("p", "vm", "k"); err != nil {
		t.Errorf("Delete idempotent expected, got %v", err)
	}
}

func TestVMPropertyRegistry_SetValidates(t *testing.T) {
	reg := newTestVMPropRegistry(t)
	cases := []struct {
		name string
		p    VMProperty
		want string
	}{
		{"empty vm_name", VMProperty{Project: "p", Key: "k"}, "vm_name"},
		{"empty key", VMProperty{VMName: "vm", Project: "p"}, "key"},
	}
	for _, c := range cases {
		err := reg.Set(c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: expected error containing %q, got %v", c.name, c.want, err)
		}
	}
}

func TestVMPropertyRegistry_PersistsAcrossReload(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadVMPropertyRegistry(context.Background(), storage)
	_ = r1.Set(VMProperty{
		VMName: "web-1", Project: "alpha", Key: "weft.boot/script",
		Value: "#!/bin/sh\necho hello\n", GuestReadable: true,
	})
	r2, err := LoadVMPropertyRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := r2.ListForVM("alpha", "web-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry after reload, got %d", len(got))
	}
	if !strings.Contains(got[0].Value, "echo hello") {
		t.Errorf("value lost across reload : %q", got[0].Value)
	}
	if !got[0].GuestReadable {
		t.Error("guest_readable lost across reload")
	}
}

func TestVMPropertyRegistry_HCLStableOrdering(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadVMPropertyRegistry(context.Background(), storage)
	// Set in random order ; the file should be sorted by
	// (project, vm, key) for diff-friendly output.
	for _, p := range []VMProperty{
		{VMName: "vm-b", Project: "alpha", Key: "owner", Value: "a"},
		{VMName: "vm-a", Project: "beta", Key: "tier", Value: "b"},
		{VMName: "vm-a", Project: "alpha", Key: "owner", Value: "c"},
	} {
		_ = r1.Set(p)
	}
	blob, _ := storage.Load(context.Background())
	text := string(blob)
	want := []string{
		`property "alpha/vm-a/owner"`,
		`property "alpha/vm-b/owner"`,
		`property "beta/vm-a/tier"`,
	}
	last := -1
	for _, w := range want {
		idx := strings.Index(text, w)
		if idx <= last {
			t.Errorf("block %q out of order : at %d, prev %d", w, idx, last)
		}
		last = idx
	}
}
