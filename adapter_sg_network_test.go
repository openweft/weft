//go:build darwin

package weft

import (
	"strings"
	"testing"
)

// newAdapterForRegistryTest builds an Adapter backed entirely by
// MemStorage so the cross-registry checks (network → SG → delete
// refusal) can be exercised without touching disk.
func newAdapterForRegistryTest(t *testing.T) *Adapter {
	t.Helper()
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	return NewWithStorage(stateDir, factory).(*Adapter)
}

func TestAdapter_SetNetworkDefaultSecurityGroups_HappyPath(t *testing.T) {
	a := newAdapterForRegistryTest(t)
	sg, err := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetNetworkDefaultSecurityGroups(n.UUID, []string{sg.UUID}); err != nil {
		t.Fatalf("attach SG to network: %v", err)
	}
	got, _ := a.NetworkByUUID(n.UUID)
	if len(got.DefaultSecurityGroups) != 1 || got.DefaultSecurityGroups[0] != sg.UUID {
		t.Errorf("DefaultSecurityGroups = %v, want [%s]", got.DefaultSecurityGroups, sg.UUID)
	}
}

func TestAdapter_SetNetworkDefaultSecurityGroups_RejectsCrossProject(t *testing.T) {
	a := newAdapterForRegistryTest(t)
	// SG lives in project p-1, network in p-2 → reference should
	// fail (multi-tenant isolation).
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p-1", Name: "web"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p-2", Name: "main", CIDR: "10.0.0.0/24"})
	err := a.SetNetworkDefaultSecurityGroups(n.UUID, []string{sg.UUID})
	if err == nil {
		t.Fatal("cross-project SG reference should be rejected")
	}
	if !strings.Contains(err.Error(), "cross-project") {
		t.Errorf("error message should mention cross-project, got: %v", err)
	}
}

func TestAdapter_SetNetworkDefaultSecurityGroups_RejectsUnknownSG(t *testing.T) {
	a := newAdapterForRegistryTest(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24"})
	if err := a.SetNetworkDefaultSecurityGroups(n.UUID, []string{"sg-does-not-exist"}); err == nil {
		t.Error("unknown SG should be rejected")
	}
}

func TestAdapter_SetNetworkDefaultSecurityGroups_RejectsDuplicate(t *testing.T) {
	a := newAdapterForRegistryTest(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "web"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24"})
	if err := a.SetNetworkDefaultSecurityGroups(n.UUID, []string{sg.UUID, sg.UUID}); err == nil {
		t.Error("duplicate SG in list should be rejected")
	}
}

func TestAdapter_DeleteSecurityGroup_RefusedWhenReferenced(t *testing.T) {
	a := newAdapterForRegistryTest(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "web"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24"})
	_ = a.SetNetworkDefaultSecurityGroups(n.UUID, []string{sg.UUID})

	// Delete must refuse while still referenced.
	err := a.DeleteSecurityGroup(sg.UUID)
	if err == nil {
		t.Fatal("delete of referenced SG should be rejected")
	}
	if !strings.Contains(err.Error(), n.UUID) {
		t.Errorf("error should name the referencing network %s, got: %v", n.UUID, err)
	}

	// Clear the reference; delete now succeeds.
	if err := a.SetNetworkDefaultSecurityGroups(n.UUID, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSecurityGroup(sg.UUID); err != nil {
		t.Fatalf("delete after clearing ref: %v", err)
	}
	if _, ok := a.SecurityGroupByUUID(sg.UUID); ok {
		t.Error("SG should be gone after delete")
	}
}

func TestAdapter_SetNetworkDefaultSecurityGroups_Clear(t *testing.T) {
	a := newAdapterForRegistryTest(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "web"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "main", CIDR: "10.0.0.0/24"})
	_ = a.SetNetworkDefaultSecurityGroups(n.UUID, []string{sg.UUID})
	// Nil clears.
	if err := a.SetNetworkDefaultSecurityGroups(n.UUID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := a.NetworkByUUID(n.UUID)
	if len(got.DefaultSecurityGroups) != 0 {
		t.Errorf("expected empty SG list after clear, got %v", got.DefaultSecurityGroups)
	}
}
