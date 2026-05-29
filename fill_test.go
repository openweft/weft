//go:build darwin

package weft

// fill_test.go: targeted tests for the last batch of partially
// covered functions — creds error paths, ScheduleVM single-replica,
// placeArtefact copy fallback, registry setName/setState rollbacks,
// ProjectMembers nil-defensive, and a couple of small edge cases.

import (
	"context"
	"testing"

	agent "github.com/openweft/weft/agent"
	drivers "github.com/openweft/weft-drivers"
)

// ── creds_jwt.go error paths (bad seeds) ───────────────────────

func TestMintAccount_BadOperatorSeed(t *testing.T) {
	if _, err := MintAccount([]byte("not-a-seed"), "acct"); err == nil {
		t.Errorf("bad operator seed should error")
	}
}

func TestMintUser_BadAccountSeed(t *testing.T) {
	if _, err := MintUser([]byte("not-a-seed"), "user", nil, nil); err == nil {
		t.Errorf("bad account seed should error")
	}
}

func TestMintUser_WithAllowLists(t *testing.T) {
	op, err := MintOperator("op")
	if err != nil {
		t.Fatal(err)
	}
	acc, err := MintAccount(op.Seed, "acct")
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the subscribe/publish allow-list branches.
	u, err := MintUser(acc.Seed, "project:abc",
		[]string{"vzd.events.project.abc.events.>"},
		[]string{"vzd.app.project.abc.>"})
	if err != nil {
		t.Fatalf("MintUser: %v", err)
	}
	if u.JWT == "" || u.Seed == nil {
		t.Errorf("user creds incomplete: %+v", u)
	}
	// FormatCredsFile round-trips.
	b, err := FormatCredsFile(u)
	if err != nil {
		t.Fatalf("FormatCredsFile: %v", err)
	}
	if len(b) == 0 {
		t.Errorf("empty creds file")
	}
}

// ── creds.go publicKeyFromSeed error path ──────────────────────

func TestPublicKeyFromSeed_BadSeed(t *testing.T) {
	if _, err := publicKeyFromSeed("not-a-real-seed"); err == nil {
		t.Errorf("bad seed should error")
	}
}

// ── ScheduleVM single-replica through the Adapter ──────────────

func TestAdapter_ScheduleVM(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Self-registered local host can satisfy an apple-vz request.
	host, err := a.ScheduleVM(context.Background(), ScheduleRequest{Hypervisor: "apple-vz"})
	if err != nil {
		t.Fatalf("ScheduleVM: %v", err)
	}
	if host.UUID == "" {
		t.Errorf("expected a host UUID")
	}
}

func TestAdapter_ScheduleVM_NilSchedulerDefaults(t *testing.T) {
	a := newAdapterForRegistries(t)
	a.scheduler = nil
	if _, err := a.ScheduleVM(context.Background(), ScheduleRequest{Hypervisor: "apple-vz"}); err != nil {
		t.Errorf("nil scheduler should default + work: %v", err)
	}
}

// placeArtefact moved into the weft-driver-vz plugin (cmd/weft-driver-vz);
// its coverage, including this dst-is-dir error path, lives in that module's
// provision_test.go now.

// ── registry setName rollback on save error ────────────────────

func TestVolumeRegistry_SetNameRollbackOnSaveError(t *testing.T) {
	mem := NewMemStorage()
	reg, _ := loadVolumeRegistry(context.Background(), mem)
	v, _ := reg.create(CreateVolumeSpec{ProjectUUID: "p", Name: "n", SizeGiB: 1})
	reg.storage = saveFailsStorage{}
	if err := reg.setName(v.UUID, "renamed"); err == nil {
		t.Fatal("expected save error")
	}
}

func TestVMRegistry_SetStateRollbackOnSaveError(t *testing.T) {
	mem := NewMemStorage()
	reg, _ := loadVMRegistry(context.Background(), mem)
	v, _ := reg.create(CreateVMSpec{ProjectUUID: "p", Name: "n", HostUUID: "h"})
	reg.storage = saveFailsStorage{}
	if err := reg.setState(v.UUID, VMStateRunning); err == nil {
		t.Fatal("expected save error")
	}
}

// ── ProjectMembers nil-defensive ───────────────────────────────

func TestAdapter_ProjectMembers_NilRegistry(t *testing.T) {
	a := &Adapter{}
	if _, ok := a.ProjectMembers("p"); ok {
		t.Errorf("nil registry should return (nil, false)")
	}
}

// ── loadOrCreateHostUUID: reads an existing file ───────────────

func TestLoadOrCreateHostUUID_ReadsExisting(t *testing.T) {
	tmp := t.TempDir()
	a := &Adapter{stateDir: tmp}
	// First call creates + persists.
	u1, err := a.loadOrCreateHostUUID()
	if err != nil {
		t.Fatal(err)
	}
	// Second call reads the same value back.
	u2, err := a.loadOrCreateHostUUID()
	if err != nil {
		t.Fatal(err)
	}
	if u1 != u2 {
		t.Errorf("UUID changed: %q vs %q", u1, u2)
	}
}

// ── NATS connect with a credentials file path option ───────────

func TestNewNATSEventBus_WithCredentialsFile(t *testing.T) {
	// CredentialsFile triggers the nats.UserCredentials option
	// branch in the constructor. Connection still fails (no
	// server), but the option-building branch is exercised.
	_, err := NewNATSEventBus(NATSConfig{
		URL:             "nats://127.0.0.1:1",
		CredentialsFile: "/var/empty/creds",
		Name:            "test",
		SubjectPrefix:   "custom.prefix",
	})
	if err == nil {
		t.Errorf("connect to dead endpoint should fail")
	}
}

// ── DeleteVM error: hypervisor lookup fails ────────────────────

func TestAdapter_DeleteVM_NoHandle(t *testing.T) {
	a := newAdapterForRegistries(t)
	a.UnregisterHostHandle(a.localHostUUID())
	a.vmReg = nil
	if err := a.DeleteVM("ghost"); err == nil {
		t.Errorf("DeleteVM without handle should error")
	}
}

func TestAdapter_StopVM_NoHandle(t *testing.T) {
	a := newAdapterForRegistries(t)
	a.UnregisterHostHandle(a.localHostUUID())
	a.vmReg = nil
	if err := a.StopVM("ghost"); err == nil {
		t.Errorf("StopVM without handle should error")
	}
}

// ── agent_cp RegisterHost / AttachDrivers / Heartbeat round-trip ─

func TestAgentCP_FullFlow(t *testing.T) {
	a := newAdapterForRegistries(t)
	cp := a.AsControlPlane()

	// RegisterHost via the agent shim (translates HostRegistration
	// → RegisterHostSpec → Adapter.RegisterHost).
	uuid, err := cp.RegisterHost(context.Background(), agent.HostRegistration{
		Hostname:     "agent-host",
		AZ:           "az1",
		Hypervisor:   "apple-vz",
		Architecture: "arm64",
		NetworkTypes: []string{"nat"},
	})
	if err != nil {
		t.Fatalf("RegisterHost: %v", err)
	}
	if uuid == "" {
		t.Fatal("expected a host UUID")
	}

	// AttachDrivers installs the HostHandle in the dispatch table.
	fake := &fakeHypervisorRecord{hostUUID: uuid}
	if err := cp.AttachDrivers(context.Background(), uuid, agent.DriverHandles{
		Hypervisor: fake,
	}); err != nil {
		t.Fatalf("AttachDrivers: %v", err)
	}
	if _, err := a.HypervisorOn(uuid); err != nil {
		t.Errorf("handle not installed: %v", err)
	}

	// Heartbeat forwards to HeartbeatHost.
	if err := cp.Heartbeat(context.Background(), uuid); err != nil {
		t.Errorf("Heartbeat: %v", err)
	}
}

// TestAgentCP_RegisterHostError covers the shim's error branch:
// an empty hostname makes the underlying Adapter.RegisterHost
// fail, so the shim returns ("", err).
func TestAgentCP_RegisterHostError(t *testing.T) {
	a := newAdapterForRegistries(t)
	cp := a.AsControlPlane()
	uuid, err := cp.RegisterHost(context.Background(), agent.HostRegistration{Hostname: ""})
	if err == nil {
		t.Errorf("empty hostname should error")
	}
	if uuid != "" {
		t.Errorf("error case should return empty UUID, got %q", uuid)
	}
}

var _ drivers.HypervisorDriver = (*fakeHypervisorRecord)(nil)
