package weft

import (
	"context"
	"strings"
	"testing"
)

// newTestFlavorRegistry returns an empty registry backed by a fresh
// in-memory Storage. Used everywhere : exercises the Storage seam
// (load + save round-trip) without touching disk.
func newTestFlavorRegistry(t *testing.T) *flavorRegistry {
	t.Helper()
	reg, err := loadFlavorRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadFlavorRegistry: %v", err)
	}
	return reg
}

func TestFlavorRegistry_FreshIsEmpty(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	if got := reg.List(); len(got) != 0 {
		t.Errorf("fresh registry should be empty, got %d entries", len(got))
	}
}

func TestFlavorRegistry_SetGetRoundtrip(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	want := Flavor{Name: "small", VCPU: 2, RAM: "4Gi", EphemeralGB: 8}
	if err := reg.Set(want); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := reg.Get("small")
	if !ok {
		t.Fatal("Get returned not-found after Set")
	}
	if got != want {
		t.Errorf("Get mismatch : got %+v, want %+v", got, want)
	}
}

func TestFlavorRegistry_ListIsSortedByName(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	for _, n := range []string{"medium", "small", "xlarge", "large"} {
		if err := reg.Set(Flavor{Name: n, VCPU: 1, RAM: "1Gi", EphemeralGB: 1}); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{}
	for _, f := range reg.List() {
		names = append(names, f.Name)
	}
	want := []string{"large", "medium", "small", "xlarge"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("List order mismatch : got %v, want %v", names, want)
			break
		}
	}
}

func TestFlavorRegistry_SetReplaces(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	_ = reg.Set(Flavor{Name: "small", VCPU: 2, RAM: "4Gi", EphemeralGB: 8})
	_ = reg.Set(Flavor{Name: "small", VCPU: 4, RAM: "8Gi", EphemeralGB: 16})
	got, _ := reg.Get("small")
	if got.VCPU != 4 || got.RAM != "8Gi" {
		t.Errorf("Set should replace : got %+v", got)
	}
}

func TestFlavorRegistry_DeleteRemoves(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	_ = reg.Set(Flavor{Name: "small", VCPU: 2, RAM: "4Gi", EphemeralGB: 8})
	if err := reg.Delete("small"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := reg.Get("small"); ok {
		t.Error("entry still present after Delete")
	}
}

func TestFlavorRegistry_DeleteMissingIsNoop(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	if err := reg.Delete("nope"); err != nil {
		t.Errorf("Delete on missing should be nil, got %v", err)
	}
}

func TestFlavorRegistry_SetValidates(t *testing.T) {
	reg := newTestFlavorRegistry(t)
	cases := []struct {
		name string
		f    Flavor
		want string // substring expected in the error
	}{
		{"empty name", Flavor{VCPU: 1, RAM: "1Gi"}, "name"},
		{"zero vcpu", Flavor{Name: "x", VCPU: 0, RAM: "1Gi"}, "vcpu"},
		{"empty ram", Flavor{Name: "x", VCPU: 1, RAM: ""}, "ram"},
		{"negative disk", Flavor{Name: "x", VCPU: 1, RAM: "1Gi", EphemeralGB: -1}, "ephemeral_gb"},
	}
	for _, c := range cases {
		err := reg.Set(c.f)
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q lacks %q", c.name, err.Error(), c.want)
		}
	}
}

func TestFlavorRegistry_PersistsThroughStorage(t *testing.T) {
	// Two registries sharing one Storage : the second sees what the
	// first wrote. Confirms saveLocked actually serialises + Load
	// reads back the same shape.
	storage := NewMemStorage()
	r1, _ := loadFlavorRegistry(context.Background(), storage)
	want := Flavor{Name: "gpu-large", VCPU: 32, RAM: "256Gi", EphemeralGB: 256, GPU: "4×H100-80G"}
	if err := r1.Set(want); err != nil {
		t.Fatal(err)
	}
	r2, err := loadFlavorRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := r2.Get("gpu-large")
	if !ok {
		t.Fatal("reloaded registry missing entry")
	}
	if got != want {
		t.Errorf("round-trip mismatch : got %+v, want %+v", got, want)
	}
}

func TestFlavorRegistry_HCLParse_RejectsMissingFields(t *testing.T) {
	// A hand-edit that drops `ram` should refuse to load — Storage
	// returns the blob, hclsimple.Decode fails clean.
	bad := []byte(`flavor "bad" {
		vcpu = 2
		ephemeral_gb = 8
	}`)
	storage := NewMemStorageWith(bad)
	if _, err := loadFlavorRegistry(context.Background(), storage); err == nil {
		t.Error("expected parse error for missing ram, got nil")
	}
}

func TestFlavorRegistry_HCLRoundTrip_StableOrdering(t *testing.T) {
	// Stable ordering = stable diffs ; Set in random order, the file
	// comes out sorted by name. Important when committing the file
	// to source control.
	storage := NewMemStorage()
	r1, _ := loadFlavorRegistry(context.Background(), storage)
	for _, f := range []Flavor{
		{Name: "small", VCPU: 2, RAM: "4Gi", EphemeralGB: 8},
		{Name: "xlarge", VCPU: 16, RAM: "64Gi", EphemeralGB: 64},
		{Name: "gpu-medium", VCPU: 8, RAM: "64Gi", EphemeralGB: 64, GPU: "1×A100-40G"},
		{Name: "large", VCPU: 8, RAM: "32Gi", EphemeralGB: 32},
	} {
		if err := r1.Set(f); err != nil {
			t.Fatal(err)
		}
	}
	blob, _ := storage.Load(context.Background())
	text := string(blob)
	// The Set order was small / xlarge / gpu-medium / large ; the
	// file should be in sorted-by-name order : gpu-medium, large,
	// small, xlarge. Cheap test : find the offsets of each block
	// label and check they're monotonic.
	want := []string{`flavor "gpu-medium"`, `flavor "large"`, `flavor "small"`, `flavor "xlarge"`}
	last := -1
	for _, w := range want {
		idx := strings.Index(text, w)
		if idx < 0 {
			t.Errorf("block %q not found in output", w)
			continue
		}
		if idx <= last {
			t.Errorf("block %q out of order : at %d, previous at %d", w, idx, last)
		}
		last = idx
	}
}
