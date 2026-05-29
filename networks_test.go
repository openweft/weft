package weft

import (
	"context"
	"strings"
	"testing"
)

func TestNetworkRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadNetworkRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadNetworkRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry has %d entries, want 0", got)
	}
	if got := reg.list(); len(got) != 0 {
		t.Errorf("list() = %d entries, want 0", len(got))
	}
}

func TestNetworkRegistry_CreateAndLookup(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, err := reg.create(CreateNetworkSpec{
		ProjectUUID: "p-1",
		Name:        "default",
		CIDR:        "10.42.0.0/24",
		Gateway:     "10.42.0.1",
		DNSServers:  []string{"1.1.1.1", "9.9.9.9"},
		Type:        NetworkTypeNAT,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.UUID == "" {
		t.Errorf("created network should have UUID")
	}
	if n.Type != NetworkTypeNAT {
		t.Errorf("type = %q, want nat", n.Type)
	}
	if got, ok := reg.lookupByUUID(n.UUID); !ok || got.UUID != n.UUID {
		t.Errorf("lookupByUUID failed: ok=%v", ok)
	}
	if got, ok := reg.lookupByName("p-1", "default"); !ok || got.UUID != n.UUID {
		t.Errorf("lookupByName failed: ok=%v", ok)
	}
}

func TestNetworkRegistry_TypeDefaultsToNAT(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, err := reg.create(CreateNetworkSpec{
		ProjectUUID: "p-1",
		Name:        "default",
		CIDR:        "10.42.0.0/24",
		// Type left empty
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if n.Type != NetworkTypeNAT {
		t.Errorf("empty type should default to nat, got %q", n.Type)
	}
}

func TestNetworkRegistry_CrossProjectSameName(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	// "default" in two different projects is fine.
	a, err := reg.create(CreateNetworkSpec{ProjectUUID: "p-1", Name: "default", CIDR: "10.1.0.0/24"})
	if err != nil {
		t.Fatalf("create p-1: %v", err)
	}
	b, err := reg.create(CreateNetworkSpec{ProjectUUID: "p-2", Name: "default", CIDR: "10.2.0.0/24"})
	if err != nil {
		t.Fatalf("create p-2: %v", err)
	}
	if a.UUID == b.UUID {
		t.Errorf("two networks should have distinct UUIDs")
	}
	// Each lookup resolves to its own network.
	gotA, _ := reg.lookupByName("p-1", "default")
	gotB, _ := reg.lookupByName("p-2", "default")
	if gotA.UUID != a.UUID || gotB.UUID != b.UUID {
		t.Errorf("cross-project name resolution wrong: a=%q b=%q", gotA.UUID, gotB.UUID)
	}
}

func TestNetworkRegistry_SameProjectNameCollision(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	if _, err := reg.create(CreateNetworkSpec{ProjectUUID: "p-1", Name: "n", CIDR: "10.1.0.0/24"}); err != nil {
		t.Fatal(err)
	}
	_, err := reg.create(CreateNetworkSpec{ProjectUUID: "p-1", Name: "n", CIDR: "10.2.0.0/24"})
	if err == nil {
		t.Errorf("duplicate name in same project should be rejected")
	}
}

func TestNetworkRegistry_RejectsInvalidInput(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name string
		spec CreateNetworkSpec
	}{
		{"empty project", CreateNetworkSpec{Name: "n", CIDR: "10.0.0.0/24"}},
		{"empty name", CreateNetworkSpec{ProjectUUID: "p", CIDR: "10.0.0.0/24"}},
		{"bad cidr", CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "not-a-cidr"}},
		{"gateway outside cidr", CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24", Gateway: "192.168.1.1"}},
		{"bad gateway IP", CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24", Gateway: "not-an-ip"}},
		{"unknown type", CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24", Type: NetworkType("turbo")}},
	}
	for _, tc := range cases {
		if _, err := reg.create(tc.spec); err == nil {
			t.Errorf("case %q: should be rejected, got nil error", tc.name)
		}
	}
}

func TestNetworkRegistry_SetName(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, _ := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "old", CIDR: "10.0.0.0/24"})
	if err := reg.setName(n.UUID, "new"); err != nil {
		t.Fatalf("setName: %v", err)
	}
	// Old name no longer resolves; new name does.
	if _, ok := reg.lookupByName("p", "old"); ok {
		t.Errorf("old name still resolves after rename")
	}
	got, ok := reg.lookupByName("p", "new")
	if !ok || got.UUID != n.UUID {
		t.Errorf("new name doesn't resolve")
	}
	// Empty name rejected.
	if err := reg.setName(n.UUID, ""); err == nil {
		t.Errorf("empty name should be rejected")
	}
	// Unknown UUID rejected.
	if err := reg.setName("nope", "x"); err == nil {
		t.Errorf("unknown UUID should be rejected")
	}
	// Rename to existing name in same project rejected.
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "other", CIDR: "10.1.0.0/24"})
	if err := reg.setName(n.UUID, "other"); err == nil {
		t.Errorf("rename to existing name should be rejected")
	}
}

func TestNetworkRegistry_SetDNSServers(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, _ := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	if err := reg.setDNSServers(n.UUID, []string{"8.8.8.8", "8.8.4.4"}); err != nil {
		t.Fatalf("setDNSServers: %v", err)
	}
	got, _ := reg.lookupByUUID(n.UUID)
	if len(got.DNSServers) != 2 || got.DNSServers[0] != "8.8.8.8" {
		t.Errorf("DNS servers not updated: %v", got.DNSServers)
	}
	// Clear via nil.
	if err := reg.setDNSServers(n.UUID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = reg.lookupByUUID(n.UUID)
	if len(got.DNSServers) != 0 {
		t.Errorf("DNS servers should be cleared: %v", got.DNSServers)
	}
}

func TestNetworkRegistry_Delete(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, _ := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	if err := reg.delete(n.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.lookupByUUID(n.UUID); ok {
		t.Errorf("network should be gone after delete")
	}
	if _, ok := reg.lookupByName("p", "n"); ok {
		t.Errorf("name index should be gone after delete")
	}
	// project index entry should be cleaned up.
	if _, ok := reg.projectIdx["p"]; ok {
		t.Errorf("project index entry should be removed when last network deleted")
	}
	// Re-creating the same name now works.
	if _, err := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"}); err != nil {
		t.Errorf("recreate after delete should succeed: %v", err)
	}
	// Delete unknown UUID rejected.
	if err := reg.delete("nope"); err == nil {
		t.Errorf("delete unknown UUID should be rejected")
	}
}

func TestNetworkRegistry_ListForProject(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p-1", Name: "alpha", CIDR: "10.1.0.0/24"})
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p-1", Name: "beta", CIDR: "10.2.0.0/24"})
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p-2", Name: "gamma", CIDR: "10.3.0.0/24"})

	got := reg.listForProject("p-1")
	if len(got) != 2 {
		t.Fatalf("listForProject(p-1) size = %d, want 2", len(got))
	}
	// Sorted by name: alpha before beta.
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("listForProject not sorted by name: %v", []string{got[0].Name, got[1].Name})
	}
	// p-2 has just one.
	if g := reg.listForProject("p-2"); len(g) != 1 || g[0].Name != "gamma" {
		t.Errorf("listForProject(p-2) wrong: %v", g)
	}
	// Unknown project → empty.
	if g := reg.listForProject("nope"); len(g) != 0 {
		t.Errorf("listForProject(unknown) should be empty, got %v", g)
	}
}

func TestNetworkRegistry_CreateMesh(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, err := reg.create(CreateNetworkSpec{
		ProjectUUID:    "p",
		Name:           "wg0",
		CIDR:           "10.100.0.0/24",
		Type:           NetworkTypeMesh,
		MeshListenPort: 51820,
		MeshEndpoint:   "mesh.example.com:51820",
	})
	if err != nil {
		t.Fatalf("create mesh: %v", err)
	}
	if n.Type != NetworkTypeMesh {
		t.Errorf("type = %q, want mesh", n.Type)
	}
	if n.MeshListenPort != 51820 {
		t.Errorf("listen port = %d, want 51820", n.MeshListenPort)
	}
	if n.MeshEndpoint != "mesh.example.com:51820" {
		t.Errorf("endpoint wrong: %q", n.MeshEndpoint)
	}
}

func TestNetworkRegistry_RejectsMeshFieldsOnNonMesh(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	cases := []CreateNetworkSpec{
		{ProjectUUID: "p", Name: "a", CIDR: "10.0.0.0/24", Type: NetworkTypeNAT, MeshListenPort: 51820},
		{ProjectUUID: "p", Name: "b", CIDR: "10.0.0.0/24", Type: NetworkTypeBridged, MeshEndpoint: "host:51820"},
		// Empty type defaults to NAT — mesh-field still rejected.
		{ProjectUUID: "p", Name: "c", CIDR: "10.0.0.0/24", MeshEndpoint: "host:51820"},
	}
	for i, spec := range cases {
		if _, err := reg.create(spec); err == nil {
			t.Errorf("case %d: mesh field on non-mesh type should be rejected", i)
		}
	}
}

func TestNetworkRegistry_RejectsInvalidMeshFields(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name string
		spec CreateNetworkSpec
	}{
		{
			"port out of range",
			CreateNetworkSpec{ProjectUUID: "p", Name: "a", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh, MeshListenPort: 99999},
		},
		{
			"negative port",
			CreateNetworkSpec{ProjectUUID: "p", Name: "b", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh, MeshListenPort: -1},
		},
		{
			"endpoint no port",
			CreateNetworkSpec{ProjectUUID: "p", Name: "c", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh, MeshEndpoint: "mesh.example.com"},
		},
		{
			"endpoint bad port",
			CreateNetworkSpec{ProjectUUID: "p", Name: "d", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh, MeshEndpoint: "mesh.example.com:99999"},
		},
		{
			"endpoint empty host",
			CreateNetworkSpec{ProjectUUID: "p", Name: "e", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh, MeshEndpoint: ":51820"},
		},
	}
	for _, tc := range cases {
		if _, err := reg.create(tc.spec); err == nil {
			t.Errorf("case %q: should be rejected", tc.name)
		}
	}
}

func TestNetworkRegistry_MeshZeroPortAllowed(t *testing.T) {
	// port 0 = "kernel picks" is valid for peers that only dial out.
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	_, err := reg.create(CreateNetworkSpec{
		ProjectUUID: "p", Name: "n", CIDR: "10.100.0.0/24",
		Type: NetworkTypeMesh, MeshListenPort: 0,
	})
	if err != nil {
		t.Errorf("port 0 should be allowed on mesh: %v", err)
	}
}

func TestNetworkRegistry_SetDefaultSecurityGroups(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	n, _ := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	if err := reg.setDefaultSecurityGroups(n.UUID, []string{"sg-1", "sg-2"}); err != nil {
		t.Fatalf("setDefaultSecurityGroups: %v", err)
	}
	got, _ := reg.lookupByUUID(n.UUID)
	if len(got.DefaultSecurityGroups) != 2 {
		t.Errorf("default-SG count = %d, want 2", len(got.DefaultSecurityGroups))
	}
	// Clear via nil.
	if err := reg.setDefaultSecurityGroups(n.UUID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = reg.lookupByUUID(n.UUID)
	if len(got.DefaultSecurityGroups) != 0 {
		t.Errorf("default-SGs should be cleared: %v", got.DefaultSecurityGroups)
	}
	// Unknown network rejected.
	if err := reg.setDefaultSecurityGroups("nope", []string{"sg-1"}); err == nil {
		t.Errorf("unknown network should be rejected")
	}
}

func TestNetworkRegistry_NetworksReferencingSecurityGroup(t *testing.T) {
	reg, _ := loadNetworkRegistry(context.Background(), NewMemStorage())
	a, _ := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "a", CIDR: "10.1.0.0/24"})
	b, _ := reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "b", CIDR: "10.2.0.0/24"})
	_, _ = reg.create(CreateNetworkSpec{ProjectUUID: "p", Name: "c", CIDR: "10.3.0.0/24"}) // no SG ref

	_ = reg.setDefaultSecurityGroups(a.UUID, []string{"sg-web", "sg-shared"})
	_ = reg.setDefaultSecurityGroups(b.UUID, []string{"sg-shared"})

	got := reg.networksReferencingSecurityGroup("sg-shared")
	if len(got) != 2 {
		t.Errorf("sg-shared refs count = %d, want 2 (got %v)", len(got), got)
	}
	got = reg.networksReferencingSecurityGroup("sg-web")
	if len(got) != 1 || got[0] != a.UUID {
		t.Errorf("sg-web refs = %v, want [%s]", got, a.UUID)
	}
	got = reg.networksReferencingSecurityGroup("sg-unknown")
	if len(got) != 0 {
		t.Errorf("unknown SG should have zero refs, got %v", got)
	}
}

// TestNetworkRegistry_RoundTripViaStorage confirms HCL encode + decode
// + every index rebuild correctly via Storage.
func TestNetworkRegistry_RoundTripViaStorage(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadNetworkRegistry(context.Background(), storage)
	a, _ := reg.create(CreateNetworkSpec{
		ProjectUUID: "p-1",
		Name:        "default",
		CIDR:        "10.42.0.0/24",
		Gateway:     "10.42.0.1",
		DNSServers:  []string{"1.1.1.1"},
		Type:        NetworkTypeNAT,
	})
	b, _ := reg.create(CreateNetworkSpec{
		ProjectUUID: "p-2",
		Name:        "lab",
		CIDR:        "192.168.50.0/24",
		Type:        NetworkTypeBridged,
	})
	// Third network exercises the mesh fields through the
	// HCL round-trip.
	c, _ := reg.create(CreateNetworkSpec{
		ProjectUUID:    "p-3",
		Name:           "wg-prod",
		CIDR:           "10.100.0.0/16",
		Type:           NetworkTypeMesh,
		MeshListenPort: 51820,
		MeshEndpoint:   "mesh.example.com:51820",
	})
	// Attach default SGs to network a so we exercise the new
	// field through HCL encode → decode.
	if err := reg.setDefaultSecurityGroups(a.UUID, []string{"sg-web", "sg-shared"}); err != nil {
		t.Fatalf("setDefaultSecurityGroups: %v", err)
	}

	// Sanity: HCL blob has the right shape.
	blob, _ := storage.Load(context.Background())
	for _, want := range []string{
		"network \"" + a.UUID + "\"",
		"network \"" + b.UUID + "\"",
		"10.42.0.0/24",
		"192.168.50.0/24",
		"bridged",
		"default_security_groups",
		"sg-web",
		"mesh_listen_port",
		"mesh_endpoint",
		"mesh.example.com:51820",
	} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("HCL missing %q: %s", want, blob)
		}
	}

	// Fresh registry from same Storage: re-resolves every entry +
	// every index.
	reg2, err := loadNetworkRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	gotA, ok := reg2.lookupByUUID(a.UUID)
	if !ok || gotA.CIDR != "10.42.0.0/24" || gotA.Gateway != "10.42.0.1" || len(gotA.DNSServers) != 1 {
		t.Errorf("a re-load wrong: %+v ok=%v", gotA, ok)
	}
	if len(gotA.DefaultSecurityGroups) != 2 || gotA.DefaultSecurityGroups[0] != "sg-web" {
		t.Errorf("default-SGs didn't survive reload: %v", gotA.DefaultSecurityGroups)
	}
	gotB, ok := reg2.lookupByUUID(b.UUID)
	if !ok || gotB.Type != NetworkTypeBridged {
		t.Errorf("b re-load wrong: %+v ok=%v", gotB, ok)
	}
	gotC, ok := reg2.lookupByUUID(c.UUID)
	if !ok || gotC.Type != NetworkTypeMesh || gotC.MeshListenPort != 51820 || gotC.MeshEndpoint != "mesh.example.com:51820" {
		t.Errorf("c (mesh) re-load wrong: %+v ok=%v", gotC, ok)
	}
	// name index re-built.
	if got, ok := reg2.lookupByName("p-1", "default"); !ok || got.UUID != a.UUID {
		t.Errorf("name index didn't survive reload")
	}
	// project index re-built.
	if got := reg2.listForProject("p-1"); len(got) != 1 || got[0].UUID != a.UUID {
		t.Errorf("project index didn't survive reload: %v", got)
	}
}
