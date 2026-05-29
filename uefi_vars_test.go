package weft

import (
	"context"
	"strings"
	"testing"
)

func newTestUEFIRegistry(t *testing.T) *uefiVarRegistry {
	t.Helper()
	reg, err := LoadUEFIVarRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("LoadUEFIVarRegistry: %v", err)
	}
	return reg
}

func TestUEFIVarRegistry_FreshEmpty(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	if got := reg.ListForVM("p", "vm"); len(got) != 0 {
		t.Errorf("fresh should be empty, got %d", len(got))
	}
}

func TestUEFIVarRegistry_SetListRoundtrip(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	nvRtBs := []string{"NonVolatile", "BootServiceAccess", "RuntimeAccess"}
	for _, v := range []UEFIVar{
		{VMName: "web-1", Project: "alpha", Name: "BootOrder", ValueHex: "0000", Attributes: nvRtBs},
		{VMName: "web-1", Project: "alpha", Name: "Boot0000", ValueHex: "0100", Attributes: nvRtBs},
		{VMName: "web-1", Project: "alpha", Name: "SecureBoot", ValueHex: "01"},
	} {
		if err := reg.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	got := reg.ListForVM("alpha", "web-1")
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	// All defaulted to EFI Global namespace.
	for _, v := range got {
		if v.Namespace != EFIGlobalNS {
			t.Errorf("empty namespace not defaulted : %q", v.Namespace)
		}
		if v.UpdatedAt.IsZero() {
			t.Errorf("UpdatedAt zero for %s", v.Name)
		}
	}
}

func TestUEFIVarRegistry_ValueHexValidation(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	bad := []struct {
		name, hex string
	}{
		{"odd-length", "0"},
		{"non-hex", "0z"},
		{"trailing garbage", "00 nope"},
	}
	for _, c := range bad {
		err := reg.Set(UEFIVar{VMName: "vm", Project: "p", Name: "X", ValueHex: c.hex})
		if err == nil {
			t.Errorf("%s : expected error for value_hex %q", c.name, c.hex)
		}
	}
}

func TestUEFIVarRegistry_EmptyValueHexAccepted(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	if err := reg.Set(UEFIVar{VMName: "vm", Project: "p", Name: "Empty"}); err != nil {
		t.Errorf("empty value_hex should be allowed, got %v", err)
	}
}

func TestUEFIVarRegistry_SpacesInHexStripped(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	if err := reg.Set(UEFIVar{
		VMName: "vm", Project: "p", Name: "X",
		ValueHex: "01 00 00 00 58 00",
	}); err != nil {
		t.Fatalf("operator-friendly spaces should be stripped: %v", err)
	}
	got := reg.ListForVM("p", "vm")
	if got[0].ValueHex != "010000005800" {
		t.Errorf("stripped value mismatch : %q", got[0].ValueHex)
	}
}

func TestUEFIVarRegistry_NamespaceUniqueness(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	// Same name in two different namespaces must coexist.
	_ = reg.Set(UEFIVar{VMName: "vm", Project: "p", Namespace: EFIGlobalNS, Name: "X", ValueHex: "01"})
	_ = reg.Set(UEFIVar{VMName: "vm", Project: "p", Namespace: "11111111-1111-1111-1111-111111111111", Name: "X", ValueHex: "02"})
	got := reg.ListForVM("p", "vm")
	if len(got) != 2 {
		t.Errorf("expected 2 entries across namespaces, got %d", len(got))
	}
}

func TestUEFIVarRegistry_DeleteIdempotent(t *testing.T) {
	reg := newTestUEFIRegistry(t)
	_ = reg.Set(UEFIVar{VMName: "vm", Project: "p", Name: "X", ValueHex: "00"})
	if err := reg.Delete("p", "vm", "", "X"); err != nil {
		t.Fatal(err)
	}
	if got := reg.ListForVM("p", "vm"); len(got) != 0 {
		t.Errorf("delete did not remove : %+v", got)
	}
	if err := reg.Delete("p", "vm", "", "X"); err != nil {
		t.Errorf("Delete idempotent expected, got %v", err)
	}
}

func TestUEFIVarRegistry_PersistsAcrossReload(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadUEFIVarRegistry(context.Background(), storage)
	_ = r1.Set(UEFIVar{
		VMName: "vm", Project: "p", Name: "BootOrder",
		ValueHex: "0000", Attributes: []string{"NonVolatile", "BootServiceAccess"},
	})
	r2, err := LoadUEFIVarRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := r2.ListForVM("p", "vm")
	if len(got) != 1 {
		t.Fatalf("expected 1, got %d", len(got))
	}
	if got[0].ValueHex != "0000" {
		t.Errorf("value lost across reload")
	}
	if len(got[0].Attributes) != 2 {
		t.Errorf("attributes lost across reload : %v", got[0].Attributes)
	}
}

func TestUEFIVarRegistry_HCLStableOrdering(t *testing.T) {
	storage := NewMemStorage()
	r1, _ := LoadUEFIVarRegistry(context.Background(), storage)
	for _, v := range []UEFIVar{
		{VMName: "vm-z", Project: "alpha", Name: "BootOrder", ValueHex: "00"},
		{VMName: "vm-a", Project: "beta", Name: "X", ValueHex: "00"},
		{VMName: "vm-a", Project: "alpha", Name: "Y", ValueHex: "00"},
	} {
		_ = r1.Set(v)
	}
	blob, _ := storage.Load(context.Background())
	text := string(blob)
	want := []string{
		`uefi_var "alpha/vm-a/`,
		`uefi_var "alpha/vm-z/`,
		`uefi_var "beta/vm-a/`,
	}
	last := -1
	for _, w := range want {
		idx := strings.Index(text, w)
		if idx <= last {
			t.Errorf("block %q out of order", w)
		}
		last = idx
	}
}
