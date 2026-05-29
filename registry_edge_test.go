//go:build darwin

package weft

// registry_edge_test.go fills the last small branches in the
// registry mutators that the existing per-resource test files
// don't exercise: invalid-state, empty-arg, not-found, same-value
// no-op, and listForProject-empty paths. Test names are unique to
// avoid clashing with the existing registry tests.

import (
	"context"
	"testing"
)

func TestVMRegistry_SetStateEdgeBranches(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	// Invalid state value (validateVMState rejects).
	if err := reg.setState("any", VMState("not-a-state")); err == nil {
		t.Errorf("invalid state should error")
	}
	// Unknown UUID with a *valid* state hits the not-found branch.
	if err := reg.setState("nope", VMStateRunning); err == nil {
		t.Errorf("unknown UUID should error")
	}
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"})
	// Same-state no-op.
	cur, _ := reg.lookupByUUID(v.UUID)
	if err := reg.setState(v.UUID, cur.State); err != nil {
		t.Errorf("same-state no-op should succeed: %v", err)
	}
}

func TestVMRegistry_SetNameEdgeBranches(t *testing.T) {
	reg, _ := loadVMRegistry(context.Background(), NewMemStorage())
	if err := reg.setName("nope", "x"); err == nil {
		t.Errorf("unknown UUID should error")
	}
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "orig", HostUUID: "h"})
	if err := reg.setName(v.UUID, ""); err == nil {
		t.Errorf("empty name should error")
	}
	// Self-rename no-op.
	if err := reg.setName(v.UUID, "orig"); err != nil {
		t.Errorf("self-rename no-op: %v", err)
	}
}

func TestSecurityGroupRegistry_ListForProjectEmptyAndSetNameEdge(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	if got := reg.listForProject("none"); got != nil {
		t.Errorf("empty listForProject should be nil")
	}
	if err := reg.setName("nope", "x"); err == nil {
		t.Errorf("unknown setName should error")
	}
	g, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "sg1"})
	if err := reg.setName(g.UUID, ""); err == nil {
		t.Errorf("empty name should error")
	}
	if err := reg.setName(g.UUID, "sg1"); err != nil {
		t.Errorf("self-rename no-op: %v", err)
	}
}

func TestPortRegistry_SetSecurityGroupsEdge(t *testing.T) {
	reg, _ := loadPortRegistry(context.Background(), NewMemStorage())
	if err := reg.setSecurityGroups("nope", []string{"x"}); err == nil {
		t.Errorf("unknown port should error")
	}
}
