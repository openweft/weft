package weft

import (
	"context"
	"strings"
	"testing"
)

func TestVolumeRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadVolumeRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadVolumeRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
	if got := reg.list(); len(got) != 0 {
		t.Errorf("list() = %d entries, want 0", len(got))
	}
}

func TestVolumeRegistry_CreateAndLookup(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, err := reg.create(CreateVolumeSpec{
		ProjectUUID: "p-1",
		Name:        "data",
		SizeGiB:     100,
		Format:      VolumeFormatRaw,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.UUID == "" {
		t.Errorf("created volume should have UUID")
	}
	if v.Format != VolumeFormatRaw {
		t.Errorf("format = %q, want raw", v.Format)
	}
	if v.AttachedTo != "" {
		t.Errorf("new volume should start detached")
	}
	if got, ok := reg.lookupByUUID(v.UUID); !ok || got.UUID != v.UUID {
		t.Errorf("lookupByUUID failed: ok=%v", ok)
	}
	if got, ok := reg.lookupByName("p-1", "data"); !ok || got.UUID != v.UUID {
		t.Errorf("lookupByName failed: ok=%v", ok)
	}
}

func TestVolumeRegistry_FormatDefaultsToRaw(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, err := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: 10})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.Format != VolumeFormatRaw {
		t.Errorf("empty format should default to raw, got %q", v.Format)
	}
}

func TestVolumeRegistry_CrossProjectSameName(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	a, err := reg.create(CreateVolumeSpec{ProjectUUID: "p-1", Name: "data", SizeGiB: 10})
	if err != nil {
		t.Fatal(err)
	}
	b, err := reg.create(CreateVolumeSpec{ProjectUUID: "p-2", Name: "data", SizeGiB: 20})
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == b.UUID {
		t.Errorf("two volumes should have distinct UUIDs")
	}
	gotA, _ := reg.lookupByName("p-1", "data")
	gotB, _ := reg.lookupByName("p-2", "data")
	if gotA.UUID != a.UUID || gotB.UUID != b.UUID {
		t.Errorf("cross-project name resolution wrong")
	}
}

func TestVolumeRegistry_RejectsInvalidInput(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name string
		spec CreateVolumeSpec
	}{
		{"empty project", CreateVolumeSpec{Name: "n", SizeGiB: 10}},
		{"empty name", CreateVolumeSpec{ProjectUUID: "p", SizeGiB: 10}},
		{"zero size", CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: 0}},
		{"negative size", CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: -5}},
		{"unknown format", CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: 10, Format: VolumeFormat("vmdk")}},
	}
	for _, tc := range cases {
		if _, err := reg.create(tc.spec); err == nil {
			t.Errorf("case %q: should be rejected", tc.name)
		}
	}
}

func TestVolumeRegistry_SameProjectNameCollision(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	if _, err := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "v", SizeGiB: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "v", SizeGiB: 20}); err == nil {
		t.Errorf("duplicate name in same project should be rejected")
	}
}

func TestVolumeRegistry_SetName(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "old", SizeGiB: 10})
	if err := reg.setName(v.UUID, "new"); err != nil {
		t.Fatalf("setName: %v", err)
	}
	if _, ok := reg.lookupByName("p", "old"); ok {
		t.Errorf("old name still resolves after rename")
	}
	if got, ok := reg.lookupByName("p", "new"); !ok || got.UUID != v.UUID {
		t.Errorf("new name doesn't resolve")
	}
	if err := reg.setName(v.UUID, ""); err == nil {
		t.Errorf("empty name should be rejected")
	}
	if err := reg.setName("nope", "x"); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
	// Collision in same project.
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "other", SizeGiB: 10})
	if err := reg.setName(v.UUID, "other"); err == nil {
		t.Errorf("rename to existing name should be rejected")
	}
}

func TestVolumeRegistry_Resize(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "v", SizeGiB: 50})
	// Grow allowed.
	if err := reg.resize(v.UUID, 100); err != nil {
		t.Fatalf("grow: %v", err)
	}
	got, _ := reg.lookupByUUID(v.UUID)
	if got.SizeGiB != 100 {
		t.Errorf("size after grow = %d, want 100", got.SizeGiB)
	}
	// Same size is a no-op.
	if err := reg.resize(v.UUID, 100); err != nil {
		t.Errorf("no-op resize should succeed: %v", err)
	}
	// Shrink refused.
	if err := reg.resize(v.UUID, 50); err == nil {
		t.Errorf("shrink should be rejected")
	}
	// Zero / negative refused.
	if err := reg.resize(v.UUID, 0); err == nil {
		t.Errorf("size 0 should be rejected")
	}
	// Unknown UUID refused.
	if err := reg.resize("nope", 200); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestVolumeRegistry_AttachDetach(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "v", SizeGiB: 10})
	if err := reg.attach(v.UUID, "vm-1"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, _ := reg.lookupByUUID(v.UUID)
	if got.AttachedTo != "vm-1" {
		t.Errorf("AttachedTo = %q, want vm-1", got.AttachedTo)
	}
	// Same-target attach is a no-op.
	if err := reg.attach(v.UUID, "vm-1"); err != nil {
		t.Errorf("repeat attach to same target should be no-op: %v", err)
	}
	// Different-target attach while still attached: rejected.
	if err := reg.attach(v.UUID, "vm-2"); err == nil {
		t.Errorf("attach while bound elsewhere should be rejected")
	}
	// Detach + re-attach to different VM works.
	if err := reg.detach(v.UUID); err != nil {
		t.Fatal(err)
	}
	got, _ = reg.lookupByUUID(v.UUID)
	if got.AttachedTo != "" {
		t.Errorf("AttachedTo after detach = %q, want empty", got.AttachedTo)
	}
	if err := reg.attach(v.UUID, "vm-2"); err != nil {
		t.Errorf("re-attach after detach should succeed: %v", err)
	}
	// Detach when already detached is a no-op.
	_ = reg.detach(v.UUID)
	if err := reg.detach(v.UUID); err != nil {
		t.Errorf("repeat detach should be no-op: %v", err)
	}
	// Empty vmUUID rejected.
	if err := reg.attach(v.UUID, ""); err == nil {
		t.Errorf("empty vm_uuid should be rejected")
	}
}

func TestVolumeRegistry_DeleteRefusesAttached(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "v", SizeGiB: 10})
	_ = reg.attach(v.UUID, "vm-1")
	if err := reg.delete(v.UUID); err == nil {
		t.Errorf("delete of attached volume should be rejected")
	}
	_ = reg.detach(v.UUID)
	if err := reg.delete(v.UUID); err != nil {
		t.Fatalf("delete after detach: %v", err)
	}
	if _, ok := reg.lookupByUUID(v.UUID); ok {
		t.Errorf("volume should be gone after delete")
	}
	if _, ok := reg.lookupByName("p", "v"); ok {
		t.Errorf("name index should be gone after delete")
	}
	if _, ok := reg.projectIdx["p"]; ok {
		t.Errorf("project index should be cleaned up")
	}
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown UUID should be rejected")
	}
}

func TestVolumeRegistry_ListForProject(t *testing.T) {
	reg, _ := loadVolumeRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p-1", Name: "alpha", SizeGiB: 10})
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p-1", Name: "beta", SizeGiB: 10})
	_, _ = reg.create(CreateVolumeSpec{ProjectUUID: "p-2", Name: "gamma", SizeGiB: 10})

	got := reg.listForProject("p-1")
	if len(got) != 2 {
		t.Fatalf("listForProject(p-1) size = %d, want 2", len(got))
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("listForProject not sorted by name: %v", []string{got[0].Name, got[1].Name})
	}
	if g := reg.listForProject("nope"); len(g) != 0 {
		t.Errorf("unknown project should return empty")
	}
}

// TestVolumeRegistry_RoundTripViaStorage confirms HCL encode + decode +
// every index rebuilds via Storage.
func TestVolumeRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadVolumeRegistry(context.Background(), storage)
	a, _ := reg.create(CreateVolumeSpec{
		ProjectUUID: "p-1",
		Name:        "data",
		SizeGiB:     100,
		Format:      VolumeFormatRaw,
	})
	b, _ := reg.create(CreateVolumeSpec{
		ProjectUUID: "p-2",
		Name:        "snap",
		SizeGiB:     250,
		Format:      VolumeFormatQCOW2,
	})
	_ = reg.attach(a.UUID, "vm-1")
	_ = reg.resize(b.UUID, 500)

	blob, _ := storage.Load(context.Background())
	for _, want := range []string{
		"volume \"" + a.UUID + "\"",
		"volume \"" + b.UUID + "\"",
		"qcow2",
		"vm-1",
		"500",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("HCL missing %q: %s", want, blob)
		}
	}

	reg2, err := loadVolumeRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	gotA, ok := reg2.lookupByUUID(a.UUID)
	if !ok || gotA.AttachedTo != "vm-1" || gotA.SizeGiB != 100 {
		t.Errorf("a re-load wrong: %+v", gotA)
	}
	gotB, ok := reg2.lookupByUUID(b.UUID)
	if !ok || gotB.SizeGiB != 500 || gotB.Format != VolumeFormatQCOW2 {
		t.Errorf("b re-load wrong: %+v", gotB)
	}
	// name + project indices re-built.
	if got, ok := reg2.lookupByName("p-1", "data"); !ok || got.UUID != a.UUID {
		t.Errorf("name index didn't survive reload")
	}
	if got := reg2.listForProject("p-2"); len(got) != 1 || got[0].UUID != b.UUID {
		t.Errorf("project index didn't survive reload: %v", got)
	}
}
