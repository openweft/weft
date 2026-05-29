package weft

import (
	"context"
	"strings"
	"testing"
)

func TestSecurityGroupRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadSecurityGroupRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
}

func TestSecurityGroupRegistry_CreateAndLookup(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	g, err := reg.create(CreateSecurityGroupSpec{
		ProjectUUID: "p-1",
		Name:        "web",
		Description: "public HTTP/HTTPS",
		Rules: []SecurityRule{
			{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 443, PortMax: 443, RemoteCIDR: "0.0.0.0/0"},
			{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 80, PortMax: 80, RemoteCIDR: "0.0.0.0/0"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if g.UUID == "" {
		t.Errorf("created group should have UUID")
	}
	if len(g.Rules) != 2 {
		t.Errorf("rules count = %d, want 2", len(g.Rules))
	}
	if got, ok := reg.lookupByUUID(g.UUID); !ok || got.UUID != g.UUID {
		t.Errorf("lookupByUUID failed: ok=%v", ok)
	}
	if got, ok := reg.lookupByName("p-1", "web"); !ok || got.UUID != g.UUID {
		t.Errorf("lookupByName failed: ok=%v", ok)
	}
}

func TestSecurityGroupRegistry_EmptyGroupAllowed(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	g, err := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"})
	if err != nil {
		t.Fatalf("create empty group: %v", err)
	}
	if len(g.Rules) != 0 {
		t.Errorf("empty group should have no rules")
	}
}

func TestSecurityGroupRegistry_RejectsInvalidRules(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name string
		rule SecurityRule
	}{
		{"bad direction", SecurityRule{Direction: "sideways", Protocol: SGProtocolTCP, PortMin: 80, PortMax: 80, RemoteCIDR: "0.0.0.0/0"}},
		{"bad protocol", SecurityRule{Direction: SGDirectionIngress, Protocol: "sctp", RemoteCIDR: "0.0.0.0/0"}},
		{"ports on icmp", SecurityRule{Direction: SGDirectionIngress, Protocol: SGProtocolICMP, PortMin: 0, PortMax: 0, RemoteCIDR: "0.0.0.0/0"}}, // No ports → OK
		{"ports out of range", SecurityRule{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 70000, PortMax: 70000, RemoteCIDR: "0.0.0.0/0"}},
		{"min > max", SecurityRule{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 100, PortMax: 50, RemoteCIDR: "0.0.0.0/0"}},
		{"neither remote", SecurityRule{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 80, PortMax: 80}},
		{"both remote", SecurityRule{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 80, PortMax: 80, RemoteCIDR: "0.0.0.0/0", RemoteGroup: "sg-1"}},
		{"bad cidr", SecurityRule{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 80, PortMax: 80, RemoteCIDR: "not-cidr"}},
	}
	for _, tc := range cases {
		_, err := reg.create(CreateSecurityGroupSpec{
			ProjectUUID: "p",
			Name:        "n-" + tc.name,
			Rules:       []SecurityRule{tc.rule},
		})
		switch tc.name {
		case "ports on icmp":
			// This rule is actually valid — empty ports + icmp + cidr.
			if err != nil {
				t.Errorf("case %q: unexpectedly rejected: %v", tc.name, err)
			}
		default:
			if err == nil {
				t.Errorf("case %q: should be rejected", tc.name)
			}
		}
	}
}

func TestSecurityGroupRegistry_PortsOnICMPRejected(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	_, err := reg.create(CreateSecurityGroupSpec{
		ProjectUUID: "p",
		Name:        "n",
		Rules: []SecurityRule{
			{Direction: SGDirectionIngress, Protocol: SGProtocolICMP, PortMin: 80, PortMax: 80, RemoteCIDR: "0.0.0.0/0"},
		},
	})
	if err == nil {
		t.Errorf("ports on icmp should be rejected")
	}
}

func TestSecurityGroupRegistry_CrossProjectSameName(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	a, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p-1", Name: "default"})
	b, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p-2", Name: "default"})
	if a.UUID == b.UUID {
		t.Errorf("UUIDs should differ across projects")
	}
}

func TestSecurityGroupRegistry_SameProjectNameCollision(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	if _, err := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"}); err == nil {
		t.Errorf("duplicate name in same project should be rejected")
	}
}

func TestSecurityGroupRegistry_SetName(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	g, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "old"})
	if err := reg.setName(g.UUID, "new"); err != nil {
		t.Fatalf("setName: %v", err)
	}
	if _, ok := reg.lookupByName("p", "old"); ok {
		t.Errorf("old name still resolves")
	}
	if got, ok := reg.lookupByName("p", "new"); !ok || got.UUID != g.UUID {
		t.Errorf("new name doesn't resolve")
	}
	if err := reg.setName(g.UUID, ""); err == nil {
		t.Errorf("empty name should be rejected")
	}
	if err := reg.setName("nope", "x"); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
}

func TestSecurityGroupRegistry_SetRules(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	g, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"})

	newRules := []SecurityRule{
		{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 22, PortMax: 22, RemoteCIDR: "10.0.0.0/8"},
		{Direction: SGDirectionEgress, Protocol: SGProtocolAny, RemoteCIDR: "0.0.0.0/0"},
	}
	if err := reg.setRules(g.UUID, newRules); err != nil {
		t.Fatalf("setRules: %v", err)
	}
	got, _ := reg.lookupByUUID(g.UUID)
	if len(got.Rules) != 2 {
		t.Errorf("rules count = %d, want 2", len(got.Rules))
	}
	// Validation runs before any write — bad rule keeps state intact.
	bad := []SecurityRule{
		{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 22, RemoteCIDR: "0.0.0.0/0"},
		{Direction: "sideways", Protocol: SGProtocolTCP, RemoteCIDR: "0.0.0.0/0"},
	}
	if err := reg.setRules(g.UUID, bad); err == nil {
		t.Errorf("invalid rule set should be rejected")
	}
	got, _ = reg.lookupByUUID(g.UUID)
	if len(got.Rules) != 2 {
		t.Errorf("rules mutated after failed setRules: %d", len(got.Rules))
	}
	// Clear via nil.
	if err := reg.setRules(g.UUID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = reg.lookupByUUID(g.UUID)
	if len(got.Rules) != 0 {
		t.Errorf("rules should be empty after clear: %d", len(got.Rules))
	}
}

func TestSecurityGroupRegistry_SetDescription(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	g, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"})
	if err := reg.setDescription(g.UUID, "DB access only"); err != nil {
		t.Fatal(err)
	}
	got, _ := reg.lookupByUUID(g.UUID)
	if got.Description != "DB access only" {
		t.Errorf("description not updated: %q", got.Description)
	}
}

func TestSecurityGroupRegistry_Delete(t *testing.T) {
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	g, _ := reg.create(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "n"})
	if err := reg.delete(g.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookupByUUID(g.UUID); ok {
		t.Errorf("group should be gone")
	}
	if _, ok := reg.lookupByName("p", "n"); ok {
		t.Errorf("name index should be gone")
	}
	if _, ok := reg.projectIdx["p"]; ok {
		t.Errorf("project index should be cleaned up")
	}
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown UUID should be rejected")
	}
}

func TestSecurityGroupRegistry_RemoteGroupOnly(t *testing.T) {
	// Rule referencing another security group (no CIDR) should
	// be accepted; cross-SG ref is the most powerful primitive.
	reg, _ := loadSecurityGroupRegistry(context.Background(), NewMemStorage())
	_, err := reg.create(CreateSecurityGroupSpec{
		ProjectUUID: "p",
		Name:        "db",
		Rules: []SecurityRule{
			{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 5432, PortMax: 5432, RemoteGroup: "sg-web-uuid"},
		},
	})
	if err != nil {
		t.Errorf("remote_group-only rule rejected: %v", err)
	}
}

// TestSecurityGroupRegistry_RoundTripViaStorage confirms HCL +
// nested rule blocks survive Storage round-trip.
func TestSecurityGroupRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadSecurityGroupRegistry(context.Background(), storage)
	g, _ := reg.create(CreateSecurityGroupSpec{
		ProjectUUID: "p-1",
		Name:        "web",
		Description: "public HTTP/HTTPS",
		Rules: []SecurityRule{
			{Direction: SGDirectionIngress, Protocol: SGProtocolTCP, PortMin: 443, PortMax: 443, RemoteCIDR: "0.0.0.0/0"},
			{Direction: SGDirectionEgress, Protocol: SGProtocolAny, RemoteCIDR: "0.0.0.0/0"},
		},
	})

	blob, _ := storage.Load(context.Background())
	for _, want := range []string{
		"security_group \"" + g.UUID + "\"",
		"rule {",
		"public HTTP/HTTPS",
		"443",
		"0.0.0.0/0",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("HCL missing %q: %s", want, blob)
		}
	}

	reg2, err := loadSecurityGroupRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	got, ok := reg2.lookupByUUID(g.UUID)
	if !ok {
		t.Fatal("group lost on re-load")
	}
	if got.Description != "public HTTP/HTTPS" {
		t.Errorf("description not preserved: %q", got.Description)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("rules count = %d, want 2", len(got.Rules))
	}
	if got.Rules[0].Protocol != SGProtocolTCP || got.Rules[0].PortMin != 443 {
		t.Errorf("rule[0] wrong: %+v", got.Rules[0])
	}
	if got.Rules[1].Direction != SGDirectionEgress || got.Rules[1].Protocol != SGProtocolAny {
		t.Errorf("rule[1] wrong: %+v", got.Rules[1])
	}
}
