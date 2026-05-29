//go:build darwin

package weft

// coverage_test.go captures the remaining branches needed to push
// past 90% on the root package. Focus areas:
//   * Pull / parallelism + non-empty list
//   * AuthorizeProject group / admin / display-name paths
//   * Adapter migrations (migrateLegacyLayout, migrateNamedProjectDirs)
//   * Init* error fallback when Storage.Load returns malformed blob
//   * DeletePort cascade event
//   * Defaults inside SetEventBus + HeartbeatHost down→active transition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── Pull: multiple images, default parallelism = len(images) ───

func TestAdapter_Pull_MultipleAndDefaultParallelism(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Use bogus refs (non-HTTPS) so the OCI path is hit. The pull
	// will fail (network unavailable) but the call should return
	// the first error without panicking.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Pull(ctx, []string{
		"ghcr.io/owner/repo-a:tag",
		"ghcr.io/owner/repo-b:tag",
	}, 0) // parallelism=0 → falls back to len(images)
	if err == nil {
		t.Errorf("expected pull error, got nil")
	}
}

func TestAdapter_Pull_WithExplicitParallelism(t *testing.T) {
	a := newAdapterForRegistries(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Pull(ctx, []string{"ghcr.io/owner/repo:tag"}, 1)
	if err == nil {
		t.Errorf("expected pull error, got nil")
	}
}

// ── AuthorizeProject: admin path, display name path, name==subject ─

func TestAuthorizeProject_AdminResolvesName(t *testing.T) {
	a := newAdapterForRegistries(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:admin",
		Issuer:  "https://dex",
		Groups:  []string{PlatformAdminGroup},
	})
	// Admin can authorise on any name, including auto-create.
	got, err := a.AuthorizeProject(ctx, "team-net")
	if err != nil {
		t.Fatalf("admin: %v", err)
	}
	if got == "" {
		t.Errorf("expected a resolved UUID")
	}
}

func TestAuthorizeProject_DisplayNameDenied(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Pre-create a project the caller does not own.
	_, _, _ = a.CreateProject("private-team")
	outsiderCtx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:outsider",
		Issuer:  "https://dex",
	})
	if _, err := a.AuthorizeProject(outsiderCtx, "private-team"); err == nil {
		t.Errorf("outsider should be denied on private-team")
	}
}

func TestAuthorizeProject_DisplayNameOwnedViaGroup(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("group-team")
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:user",
		Issuer:  "https://dex",
		Groups:  []string{ProjectGroup(p.UUID)},
	})
	got, err := a.AuthorizeProject(ctx, "group-team")
	if err != nil {
		t.Fatalf("group owner: %v", err)
	}
	if got != p.UUID {
		t.Errorf("got %q, want %q", got, p.UUID)
	}
}

func TestAuthorizeProject_NonExistentDisplayName(t *testing.T) {
	a := newAdapterForRegistries(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:user",
		Issuer:  "https://dex",
	})
	if _, err := a.AuthorizeProject(ctx, "team-does-not-exist"); err == nil {
		t.Errorf("non-existent display name should be denied")
	}
}

// ── migrateLegacyLayout: pre-existing flat-layout VM directories ─

func TestMigrateLegacyLayout(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(tmp, factory).(*Adapter)
	// Create a flat-layout VM: <vmsDir>/oldvm with config.json + machine-id.bin.
	flatDir := filepath.Join(a.vmsDir(), "oldvm")
	if err := os.MkdirAll(flatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(flatDir, "config.json"), []byte("{}"), 0o600)
	_ = os.WriteFile(filepath.Join(flatDir, "machine-id.bin"), []byte("x"), 0o600)
	// Re-run migration — should move the VM under the default project.
	a.migrateLegacyLayout()
	// After migration the flat dir is gone.
	if _, err := os.Stat(flatDir); !os.IsNotExist(err) {
		t.Errorf("flat dir should be gone after migration")
	}
	// And it lives under <defaultProjectUUID>/oldvm.
	dst := filepath.Join(a.vmsDir(), a.DefaultProjectUUID(), "oldvm")
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("migrated VM missing: %v", err)
	}
}

// migrateNamedProjectDirs: a <name>/ subdir lookup-or-creates the project.
func TestMigrateNamedProjectDirs(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(tmp, factory).(*Adapter)
	// Create a named project subdir: <vmsDir>/named-project/somevm
	named := filepath.Join(a.vmsDir(), "named-project")
	vm := filepath.Join(named, "somevm")
	if err := os.MkdirAll(vm, 0o700); err != nil {
		t.Fatal(err)
	}
	a.migrateNamedProjectDirs()
	// Now there's a project registered as "named-project" with the VM under its UUID.
	p, ok := a.ProjectByName("named-project")
	if !ok {
		t.Fatalf("project not registered post-migration")
	}
	if _, err := os.Stat(filepath.Join(a.vmsDir(), p.UUID, "somevm")); err != nil {
		t.Errorf("VM not under UUID after migration: %v", err)
	}
}

func TestMigrateNamedProjectDirs_MergeIntoExistingDestination(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	a := NewWithStorage(tmp, factory).(*Adapter)

	// Pre-create the project so the destination UUID dir exists.
	p, _, _ := a.CreateProject("alpha")
	dst := filepath.Join(a.vmsDir(), p.UUID)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dst, "pre-existing"), []byte("x"), 0o600)

	// Now also create a named-project dir that should merge into it.
	src := filepath.Join(a.vmsDir(), "alpha")
	if err := os.MkdirAll(filepath.Join(src, "newvm"), 0o700); err != nil {
		t.Fatal(err)
	}
	a.migrateNamedProjectDirs()

	if _, err := os.Stat(filepath.Join(dst, "newvm")); err != nil {
		t.Errorf("newvm should be merged into dst: %v", err)
	}
}

// ── Init paths: malformed Storage.Load returns error → in-memory fallback ─

// failingStorage returns an error from Load and Save. The
// registry loaders MUST fall back to an empty in-memory state
// rather than crashing.
type failingStorage struct{}

func (failingStorage) Load(ctx context.Context) ([]byte, error) {
	return nil, errors.New("simulated load failure")
}
func (failingStorage) Save(ctx context.Context, blob []byte) error {
	return errors.New("simulated save failure")
}

func TestAdapter_InitsFallBackOnLoadFailure(t *testing.T) {
	tmp := t.TempDir()
	factory := func(name string) Storage { return failingStorage{} }
	// Constructor MUST NOT panic when every registry's Load fails.
	a := NewWithStorage(tmp, factory).(*Adapter)
	// All registries should be initialised (empty fallback).
	if a.projects == nil {
		t.Errorf("projects registry should be non-nil")
	}
	if a.userReg == nil {
		t.Errorf("user registry should be non-nil")
	}
	if a.networkReg == nil {
		t.Errorf("network registry should be non-nil")
	}
	if a.volumeReg == nil {
		t.Errorf("volume registry should be non-nil")
	}
	if a.sgReg == nil {
		t.Errorf("sg registry should be non-nil")
	}
	if a.portReg == nil {
		t.Errorf("port registry should be non-nil")
	}
	if a.hostReg == nil {
		t.Errorf("host registry should be non-nil")
	}
	if a.vmReg == nil {
		t.Errorf("vm registry should be non-nil")
	}
}

// ── HeartbeatHost: down → active transition publishes state_changed ─

func TestAdapter_HeartbeatHost_RevivesDown(t *testing.T) {
	a := newAdapterForRegistries(t)
	h, _ := a.RegisterHost(RegisterHostSpec{Hostname: "h-down"})
	if err := a.SetHostState(h.UUID, HostStateDown); err != nil {
		t.Fatal(err)
	}
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()
	if err := a.HeartbeatHost(h.UUID); err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, ev := range drainEvents(sub) {
		if ev.Kind == "host.state_changed" && ev.Meta["new_state"] == "active" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected host.state_changed event after heartbeat revives Down")
	}
}

// ── DeletePort missing → error ─────────────────────────────────

func TestAdapter_DeletePort_Unknown(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.DeletePort("nope"); err == nil {
		t.Errorf("delete unknown port should error")
	}
}

// ── CreatePort error paths ─────────────────────────────────────

func TestAdapter_CreatePort_UnknownNetwork(t *testing.T) {
	a := newAdapterForRegistries(t)
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p",
		NetworkUUID: "does-not-exist",
		IP:          "10.0.0.1",
	})
	if err == nil {
		t.Errorf("unknown network should error")
	}
}

func TestAdapter_CreatePort_CrossProjectNetwork(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p-1",
		Name:        "net",
		CIDR:        "10.0.0.0/24",
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p-2", // mismatched
		NetworkUUID: n.UUID,
		IP:          "10.0.0.5",
	})
	if err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Errorf("cross-project should be refused: %v", err)
	}
}

func TestAdapter_CreatePort_IPOutsideCIDR(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p",
		Name:        "net",
		CIDR:        "10.0.0.0/24",
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p",
		NetworkUUID: n.UUID,
		IP:          "192.168.1.5", // outside CIDR
	})
	if err == nil || !strings.Contains(err.Error(), "outside network cidr") {
		t.Errorf("expected outside-CIDR error: %v", err)
	}
}

func TestAdapter_CreatePort_InvalidIP(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p",
		Name:        "net",
		CIDR:        "10.0.0.0/24",
	})
	_, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p",
		NetworkUUID: n.UUID,
		IP:          "not-an-ip",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("expected invalid IP error: %v", err)
	}
}

// CreatePort mesh-field gating: a mesh network requires a wireguard
// pubkey; a non-mesh network refuses one (and refuses MeshEndpoint).
func TestAdapter_CreatePort_MeshFieldGating(t *testing.T) {
	a := newAdapterForRegistries(t)
	mesh, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "mesh", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh,
	})
	nat, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "nat", CIDR: "10.1.0.0/24", Type: NetworkTypeNAT,
	})

	// Mesh network, no pubkey → rejected.
	if _, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: mesh.UUID,
		MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
	}); err == nil {
		t.Errorf("mesh network without pubkey should be rejected")
	}

	// NAT network with a pubkey → rejected.
	if _, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: nat.UUID,
		MAC: "02:00:00:00:00:02", IP: "10.1.0.5", WireguardPubKey: "k",
	}); err == nil {
		t.Errorf("NAT network with pubkey should be rejected")
	}

	// NAT network with a mesh endpoint → rejected.
	if _, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: nat.UUID,
		MAC: "02:00:00:00:00:03", IP: "10.1.0.6", MeshEndpoint: "1.2.3.4:51820",
	}); err == nil {
		t.Errorf("NAT network with mesh endpoint should be rejected")
	}

	// Mesh network WITH pubkey → accepted.
	if _, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm", NetworkUUID: mesh.UUID,
		MAC: "02:00:00:00:00:04", IP: "10.0.0.6", WireguardPubKey: "wgkey",
	}); err != nil {
		t.Errorf("mesh network with pubkey should succeed: %v", err)
	}
}

// ── SetPortSecurityGroups error paths ────────────────────────────

func TestAdapter_SetPortSecurityGroups_UnknownPort(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.SetPortSecurityGroups("nope", nil); err == nil {
		t.Errorf("unknown port should error")
	}
}

func TestAdapter_SetPortSecurityGroups_HappyPath(t *testing.T) {
	a := newAdapterForRegistries(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "sg"})
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	port, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: n.UUID, MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}
	if err := a.SetPortSecurityGroups(port.UUID, []string{sg.UUID}); err != nil {
		t.Fatalf("setSGs: %v", err)
	}
}

// ── SetPortWireguardPubKey error paths ───────────────────────────

func TestAdapter_SetPortWireguardPubKey_Errors(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Unknown port.
	if err := a.SetPortWireguardPubKey("nope", "x"); err == nil {
		t.Errorf("unknown port should error")
	}
	// Empty pubkey on existing port.
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	port, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: n.UUID, MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}
	if err := a.SetPortWireguardPubKey(port.UUID, ""); err == nil {
		t.Errorf("empty pubkey should error")
	}
	// Non-mesh network rejects pubkey assignment.
	if err := a.SetPortWireguardPubKey(port.UUID, "wg-pubkey"); err == nil {
		t.Errorf("non-mesh network should reject pubkey")
	}
}

// ── SetPortWireguardPubKey happy path on mesh network ──────────

func TestAdapter_SetPortWireguardPubKey_MeshHappy(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p",
		Name:        "mesh",
		CIDR:        "10.0.0.0/24",
		Type:        NetworkTypeMesh,
	})
	port, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: n.UUID, MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
		WireguardPubKey: "initial-key",
	})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}
	if err := a.SetPortWireguardPubKey(port.UUID, "rotated-key"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
}

// SetPortWireguardPubKey: the port references a network that was
// since deleted → "network not found" branch.
func TestAdapter_SetPortWireguardPubKey_NetworkGone(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{
		ProjectUUID: "p", Name: "mesh", CIDR: "10.0.0.0/24", Type: NetworkTypeMesh,
	})
	port, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: n.UUID,
		MAC: "02:00:00:00:00:01", IP: "10.0.0.5", WireguardPubKey: "k0",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Delete the network out from under the port (the registry
	// doesn't cascade), then rotating the pubkey hits the
	// "network not found" branch.
	if err := a.networkReg.delete(n.UUID); err != nil {
		t.Fatal(err)
	}
	if err := a.SetPortWireguardPubKey(port.UUID, "k1"); err == nil {
		t.Errorf("expected error when port's network is gone")
	}
}

// ── DeletePort publishes peers_changed ─────────────────────────

func TestAdapter_DeletePort_PublishesPeersChanged(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	port, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: n.UUID, MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
	})
	if err != nil {
		t.Fatalf("CreatePort: %v", err)
	}
	sub, cancel := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer cancel()
	if err := a.DeletePort(port.UUID); err != nil {
		t.Fatal(err)
	}
	saw := map[string]bool{}
	for _, ev := range drainEvents(sub) {
		saw[ev.Kind] = true
	}
	if !saw["port.deleted"] {
		t.Errorf("expected port.deleted")
	}
	if !saw["network.peers_changed"] {
		t.Errorf("expected network.peers_changed cascade")
	}
}

// ── validatePortSecurityGroups dup + cross-project ─────────────

func TestAdapter_ValidatePortSecurityGroups_Duplicate(t *testing.T) {
	a := newAdapterForRegistries(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p", Name: "sg"})
	err := a.validatePortSecurityGroups([]string{sg.UUID, sg.UUID}, "p")
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("dup should error: %v", err)
	}
}

func TestAdapter_ValidatePortSecurityGroups_CrossProject(t *testing.T) {
	a := newAdapterForRegistries(t)
	sg, _ := a.CreateSecurityGroup(CreateSecurityGroupSpec{ProjectUUID: "p-other", Name: "sg"})
	if err := a.validatePortSecurityGroups([]string{sg.UUID}, "p"); err == nil {
		t.Errorf("cross-project should error")
	}
}

func TestAdapter_ValidatePortSecurityGroups_UnknownSG(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.validatePortSecurityGroups([]string{"no-such-sg"}, "p"); err == nil {
		t.Errorf("unknown SG should error")
	}
}

func TestAdapter_ValidatePortSecurityGroups_NilSGRegWithList(t *testing.T) {
	a := &Adapter{}
	if err := a.validatePortSecurityGroups([]string{"x"}, "p"); err == nil {
		t.Errorf("nil sgReg + non-empty list should error")
	}
	// Empty list is fine even with nil registry.
	if err := a.validatePortSecurityGroups(nil, "p"); err != nil {
		t.Errorf("nil sgReg + empty list should be nil: %v", err)
	}
}

// SetPortSecurityGroups: validation error (unknown SG) on a real port.
func TestAdapter_SetPortSecurityGroups_ValidationError(t *testing.T) {
	a := newAdapterForRegistries(t)
	n, _ := a.CreateNetwork(CreateNetworkSpec{ProjectUUID: "p", Name: "n", CIDR: "10.0.0.0/24"})
	port, err := a.CreatePort(CreatePortSpec{
		ProjectUUID: "p", VMUUID: "vm-1", NetworkUUID: n.UUID, MAC: "02:00:00:00:00:01", IP: "10.0.0.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetPortSecurityGroups(port.UUID, []string{"no-such-sg"}); err == nil {
		t.Errorf("unknown SG should error")
	}
}

// ── SetVMState publishes event ─────────────────────────────────

func TestAdapter_SetVMState_UnknownUUID(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.SetVMState("nope", VMStateRunning); err == nil {
		t.Errorf("unknown UUID should error")
	}
}

// ── MigrateVM error paths ──────────────────────────────────────

func TestAdapter_MigrateVM_UnknownUUID(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.MigrateVM("nope", a.localHostUUID()); err == nil {
		t.Errorf("unknown UUID should error")
	}
}

func TestAdapter_RenameVMInventory_UnknownUUID(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.RenameVMInventory("nope", "x"); err == nil {
		t.Errorf("unknown should error")
	}
}

// ── findVMByName: directory exists but is a file (non-dir) ────

func TestFindVMByName_NonDirEntry(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Plant a *file* at <vmsDir>/<somename> — findVMByName should
	// skip it gracefully.
	stray := filepath.Join(a.vmsDir(), "not-a-dir")
	_ = os.MkdirAll(a.vmsDir(), 0o755)
	_ = os.WriteFile(stray, []byte("x"), 0o600)
	if _, _, ok := a.findVMByName("anything"); ok {
		t.Errorf("findVMByName should not find anything")
	}
}

// ── VisibleProjects: no caller ─────────────────────────────────

func TestVisibleProjects_NoCaller(t *testing.T) {
	a := newAdapterForRegistries(t)
	_, _, err := a.VisibleProjects(context.Background())
	if err == nil {
		t.Errorf("no caller should error")
	}
}

// ── DeleteProject: unknown UUID via registry ───────────────────

func TestAdapter_DeleteProject_UnknownErr(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.DeleteProject("nope"); err == nil {
		t.Errorf("unknown should error")
	}
}

// ── ListLocal: cache dir doesn't exist yet ─────────────────────

func TestAdapter_ListLocal_NoVMsDir(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Brand-new adapter — vmsDir created by initProjects but with
	// no VMs. ListLocal should return an empty map.
	m, err := a.ListLocal()
	if err != nil {
		t.Fatalf("ListLocal: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty, got %d entries", len(m))
	}
}

func TestAdapter_ListLocal_AbsentDir(t *testing.T) {
	a := &Adapter{stateDir: "/var/empty/definitely-not", vmsPath: "/var/empty/definitely-not"}
	m, err := a.ListLocal()
	if err != nil {
		t.Errorf("missing dir should not error: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("expected empty, got %v", m)
	}
}

// ── SetEventBus reinstalls the hook with project routing ──────

func TestSetEventBus_ReinstallsHook(t *testing.T) {
	a := newAdapterForRegistries(t)
	newBus := NewLocalEventBus()
	a.SetEventBus(newBus)
	// RecordEvent now should land on the new bus via the bus hook.
	sub, cancel := newBus.Subscribe(EventFilter{SeeAll: true})
	defer cancel()
	vmDir := filepath.Join(a.vmsDir(), "p", "vm")
	_ = os.MkdirAll(vmDir, 0o700)
	RecordEvent(vmDir, "tester", nil)
	saw := false
	for _, ev := range drainEvents(sub) {
		if ev.Kind == "guest.tester" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("event hook didn't deliver to new bus")
	}
}

// ── DefaultProjectUUID caches its result ──────────────────────

func TestDefaultProjectUUID_Cached(t *testing.T) {
	a := newAdapterForRegistries(t)
	u1 := a.DefaultProjectUUID()
	u2 := a.DefaultProjectUUID()
	if u1 == "" || u1 != u2 {
		t.Errorf("expected stable non-empty default UUID, got %q vs %q", u1, u2)
	}
}

// DefaultProjectUUID: getOrCreate fails (failing project storage) →
// returns "" (the defensive fallback). We build an Adapter whose
// project registry can't Save.
func TestDefaultProjectUUID_GetOrCreateFails(t *testing.T) {
	preg, _ := loadProjectRegistry(context.Background(), saveFailsStorage{})
	a := &Adapter{projects: preg, bus: NewEventBus()}
	if got := a.DefaultProjectUUID(); got != "" {
		t.Errorf("getOrCreate failure should yield empty, got %q", got)
	}
}

// AuthorizeProject: empty input but the default-project getOrCreate
// fails → Internal error. Caller is non-dev, non-admin.
func TestAuthorizeProject_DefaultProjectInternalError(t *testing.T) {
	preg, _ := loadProjectRegistry(context.Background(), saveFailsStorage{})
	a := &Adapter{projects: preg, bus: NewEventBus()}
	ctx := WithCaller(context.Background(), &Caller{Subject: "ldap:x", Issuer: "https://dex"})
	if _, err := a.AuthorizeProject(ctx, ""); err == nil {
		t.Errorf("default-project mint failure should error")
	}
	// Same for the display-name == subject path.
	if _, err := a.AuthorizeProject(ctx, "ldap:x"); err == nil {
		t.Errorf("subject-name mint failure should error")
	}
}

// ResolveProjectUUID: getOrCreate fails → returns the input verbatim
// (the error fallback).
func TestResolveProjectUUID_GetOrCreateFailsReturnsInput(t *testing.T) {
	preg, _ := loadProjectRegistry(context.Background(), saveFailsStorage{})
	a := &Adapter{projects: preg}
	if got := a.ResolveProjectUUID("team-x"); got != "team-x" {
		t.Errorf("getOrCreate failure should return input, got %q", got)
	}
	// Empty input with a failing registry → DefaultProjectUUID
	// returns "" (getOrCreate failed).
	if got := a.ResolveProjectUUID(""); got != "" {
		t.Errorf("empty input with failing registry should be empty, got %q", got)
	}
}
