package weft

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestVMRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadVMRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadVMRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
}

func TestVMRegistry_Create(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	v, err := reg.create(CreateVMSpec{
		ProjectUUID: "p-1",
		Name:        "web",
		HostUUID:    "h-1",
		Image:       "ghcr.io/foo:latest",
		CPUCount:    2,
		MemoryMiB:   2048,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.UUID == "" {
		t.Errorf("created vm should have UUID")
	}
	if v.State != VMStateCreated {
		t.Errorf("initial state = %q, want created", v.State)
	}
	if got, ok := reg.lookupByUUID(v.UUID); !ok || got.UUID != v.UUID {
		t.Errorf("lookupByUUID failed")
	}
	if got, ok := reg.lookupByName("p-1", "web"); !ok || got.UUID != v.UUID {
		t.Errorf("lookupByName failed")
	}
}

func TestVMRegistry_RejectsMissingFields(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	cases := []CreateVMSpec{
		{Name: "x", HostUUID: "h"},           // empty project
		{ProjectUUID: "p", HostUUID: "h"},    // empty name
		{ProjectUUID: "p", Name: "x"},        // empty host
	}
	for i, spec := range cases {
		if _, err := reg.create(spec); err == nil {
			t.Errorf("case %d: missing field should be rejected", i)
		}
	}
}

func TestVMRegistry_PerProjectNameScope(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	a, _ := reg.create(CreateVMSpec{ProjectUUID: "p-1", Name: "web", HostUUID: "h-1"})
	b, _ := reg.create(CreateVMSpec{ProjectUUID: "p-2", Name: "web", HostUUID: "h-1"})
	if a.UUID == b.UUID {
		t.Errorf("same name in different projects must yield distinct UUIDs")
	}
	gotA, _ := reg.lookupByName("p-1", "web")
	gotB, _ := reg.lookupByName("p-2", "web")
	if gotA.UUID != a.UUID || gotB.UUID != b.UUID {
		t.Errorf("cross-project name lookup wrong")
	}
}

func TestVMRegistry_SameProjectNameCollision(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	if _, err := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"}); err == nil {
		t.Errorf("duplicate name in same project should be rejected")
	}
}

func TestVMRegistry_SetState_TracksLastStartAt(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"})
	if !v.LastStartAt.IsZero() {
		t.Errorf("LastStartAt should be zero on Create, got %v", v.LastStartAt)
	}
	t0 := time.Now()
	if err := reg.setState(v.UUID, VMStateRunning); err != nil {
		t.Fatalf("setState: %v", err)
	}
	got, _ := reg.lookupByUUID(v.UUID)
	if got.State != VMStateRunning {
		t.Errorf("state = %q, want running", got.State)
	}
	if got.LastStartAt.Before(t0) {
		t.Errorf("LastStartAt should be updated on transition to running; got %v t0=%v", got.LastStartAt, t0)
	}
	// Stopped does not touch LastStartAt.
	startedAt := got.LastStartAt
	_ = reg.setState(v.UUID, VMStateStopped)
	got, _ = reg.lookupByUUID(v.UUID)
	if got.LastStartAt != startedAt {
		t.Errorf("LastStartAt changed on stop: %v → %v", startedAt, got.LastStartAt)
	}
	// Invalid state rejected.
	if err := reg.setState(v.UUID, VMState("zombie")); err == nil {
		t.Errorf("invalid state should be rejected")
	}
	// Unknown UUID rejected.
	if err := reg.setState("nope", VMStateRunning); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestVMRegistry_SetHost_UpdatesIndexes(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h-1"})
	if err := reg.setHost(v.UUID, "h-2"); err != nil {
		t.Fatalf("setHost: %v", err)
	}
	got, _ := reg.lookupByUUID(v.UUID)
	if got.HostUUID != "h-2" {
		t.Errorf("HostUUID not updated: %q", got.HostUUID)
	}
	if g := reg.listForHost("h-1"); len(g) != 0 {
		t.Errorf("old host index should be cleared, got %d entries", len(g))
	}
	if g := reg.listForHost("h-2"); len(g) != 1 {
		t.Errorf("new host index should have 1 entry, got %d", len(g))
	}
	if err := reg.setHost(v.UUID, ""); err == nil {
		t.Errorf("empty host_uuid should be rejected")
	}
}

func TestVMRegistry_SetName(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "old", HostUUID: "h"})
	if err := reg.setName(v.UUID, "new"); err != nil {
		t.Fatalf("setName: %v", err)
	}
	if _, ok := reg.lookupByName("p", "old"); ok {
		t.Errorf("old name still resolves after rename")
	}
	if got, ok := reg.lookupByName("p", "new"); !ok || got.UUID != v.UUID {
		t.Errorf("new name doesn't resolve")
	}
	// Collision with another VM in same project.
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p", Name: "other", HostUUID: "h"})
	if err := reg.setName(v.UUID, "other"); err == nil {
		t.Errorf("rename to existing name should be rejected")
	}
}

func TestVMRegistry_Delete(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"})
	if err := reg.delete(v.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookupByUUID(v.UUID); ok {
		t.Errorf("vm should be gone after delete")
	}
	if _, ok := reg.lookupByName("p", "n"); ok {
		t.Errorf("name index should be cleared")
	}
	if g := reg.listForHost("h"); len(g) != 0 {
		t.Errorf("host index should be cleared")
	}
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown UUID should be rejected")
	}
}

func TestVMRegistry_ListForProjectAndHost(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p-1", Name: "a", HostUUID: "h-1"})
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p-1", Name: "b", HostUUID: "h-2"})
	_, _ = reg.create(CreateVMSpec{ProjectUUID: "p-2", Name: "c", HostUUID: "h-1"})

	if g := reg.listForProject("p-1"); len(g) != 2 || g[0].Name != "a" || g[1].Name != "b" {
		t.Errorf("listForProject(p-1) wrong: %v", g)
	}
	if g := reg.listForHost("h-1"); len(g) != 2 {
		t.Errorf("listForHost(h-1) = %d, want 2", len(g))
	}
	if g := reg.listForHost("nope"); len(g) != 0 {
		t.Errorf("unknown host should be empty")
	}
}

// TestVMRegistry_RoundTripViaStorage confirms HCL encode/decode
// + every index rebuild correctly.
func TestVMRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadVMRegistry(context.Background(), storage)
	v, _ := reg.create(CreateVMSpec{
		ProjectUUID: "p-1",
		Name:        "web",
		HostUUID:    "h-prod-01",
		Image:       "ghcr.io/foo:v1",
		CPUCount:    4,
		MemoryMiB:   8192,
	})
	_ = reg.setState(v.UUID, VMStateRunning)

	blob, _ := storage.Load(context.Background())
	// HCL writer aligns the `=` per block so we substring-match on
	// the operand side only (host_uuid + image values, the cpu/mem
	// numbers, the state literal). The block header carries the UUID.
	for _, want := range []string{
		"vm \"" + v.UUID + "\"",
		"\"h-prod-01\"",
		"\"ghcr.io/foo:v1\"",
		" 4\n",
		" 8192\n",
		"\"running\"",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("HCL missing %q: %s", want, blob)
		}
	}

	reg2, err := loadVMRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	got, ok := reg2.lookupByUUID(v.UUID)
	if !ok {
		t.Fatal("vm lost on reload")
	}
	if got.HostUUID != "h-prod-01" || got.Image != "ghcr.io/foo:v1" || got.State != VMStateRunning {
		t.Errorf("vm fields not preserved: %+v", got)
	}
	if got.CPUCount != 4 || got.MemoryMiB != 8192 {
		t.Errorf("vm CPU/mem not preserved: %+v", got)
	}
	if got.LastStartAt.IsZero() {
		t.Errorf("LastStartAt not preserved")
	}
	// Indices re-built.
	if _, ok := reg2.lookupByName("p-1", "web"); !ok {
		t.Errorf("name index didn't survive reload")
	}
	if g := reg2.listForHost("h-prod-01"); len(g) != 1 {
		t.Errorf("host index didn't survive reload: %d", len(g))
	}
}
