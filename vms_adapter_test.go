//go:build darwin

package weft

import (
	"strings"
	"testing"
)

func newAdapterForVMTest(t *testing.T) *Adapter {
	t.Helper()
	mockDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	return NewWithStorage(mockDir, factory).(*Adapter)
}

func TestAdapter_RegisterVM_HappyPath(t *testing.T) {
	a := newAdapterForVMTest(t)
	// The self-registered local host is the only candidate. Use
	// its UUID directly + create a project so the cross-registry
	// validation passes.
	p, _, err := a.CreateProject("acme")
	if err != nil {
		t.Fatal(err)
	}
	hostUUID := a.localHostUUID()

	v, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID,
		Name:        "web-01",
		HostUUID:    hostUUID,
		Image:       "ghcr.io/foo:v1",
		CPUCount:    2,
		MemoryMiB:   2048,
	})
	if err != nil {
		t.Fatalf("RegisterVM: %v", err)
	}
	if v.UUID == "" {
		t.Errorf("registered vm should have UUID")
	}
	if v.State != VMStateCreated {
		t.Errorf("initial state = %q, want created", v.State)
	}
	got, ok := a.VMByName(p.UUID, "web-01")
	if !ok || got.UUID != v.UUID {
		t.Errorf("VMByName lookup failed")
	}
	if g := a.ListVMsForHost(hostUUID); len(g) != 1 {
		t.Errorf("ListVMsForHost expected 1, got %d", len(g))
	}
}

func TestAdapter_RegisterVM_RejectsUnknownProject(t *testing.T) {
	a := newAdapterForVMTest(t)
	_, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: "does-not-exist",
		Name:        "web",
		HostUUID:    a.localHostUUID(),
	})
	if err == nil {
		t.Fatal("unknown project should be rejected")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("error should mention project: %v", err)
	}
}

func TestAdapter_RegisterVM_RejectsUnknownHost(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	_, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID,
		Name:        "web",
		HostUUID:    "fictional-host",
	})
	if err == nil {
		t.Fatal("unknown host should be rejected")
	}
}

func TestAdapter_RegisterVM_RejectsHostWithoutDriverHandle(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	// Register a host without giving it a driver handle.
	h, err := a.RegisterHost(RegisterHostSpec{Hostname: "headless-host"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID,
		Name:        "web",
		HostUUID:    h.UUID,
	})
	if err == nil {
		t.Fatal("host without driver handle should be rejected")
	}
	if !strings.Contains(err.Error(), "no driver handle") {
		t.Errorf("error should mention missing driver handle: %v", err)
	}
}

func TestAdapter_MigrateVM(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	// Register a second host + give it a fake driver handle so
	// MigrateVM's cross-registry check passes.
	h2, _ := a.RegisterHost(RegisterHostSpec{Hostname: "h2"})
	fake := &fakeHypervisor{hostUUID: h2.UUID}
	_ = a.RegisterHostHandle(h2.UUID, &HostHandle{Hypervisor: fake})

	v, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID, Name: "n", HostUUID: a.localHostUUID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.MigrateVM(v.UUID, h2.UUID); err != nil {
		t.Fatalf("MigrateVM: %v", err)
	}
	got, _ := a.VMByUUID(v.UUID)
	if got.HostUUID != h2.UUID {
		t.Errorf("HostUUID after migrate = %q, want %q", got.HostUUID, h2.UUID)
	}
	// Old host's index emptied, new host's index has 1 entry.
	if g := a.ListVMsForHost(a.localHostUUID()); len(g) != 0 {
		t.Errorf("old host should have no VMs, got %d", len(g))
	}
	if g := a.ListVMsForHost(h2.UUID); len(g) != 1 {
		t.Errorf("new host should have 1 VM, got %d", len(g))
	}
	// Migrating to a host without a driver handle is rejected.
	if err := a.MigrateVM(v.UUID, "fictional"); err == nil {
		t.Errorf("migrate to unknown host should be rejected")
	}
}

func TestAdapter_SetVMState_PublishesEvent(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	v, _ := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID, Name: "n", HostUUID: a.localHostUUID(),
	})
	sub, unsubscribe := a.EventBus().Subscribe(EventFilter{SeeAll: true})
	defer unsubscribe()
	if err := a.SetVMState(v.UUID, VMStateRunning); err != nil {
		t.Fatalf("SetVMState: %v", err)
	}
	// Drain looking for vm.state_changed.
	sawTransition := false
	for i := 0; i < 10; i++ {
		select {
		case ev := <-sub:
			if ev.Kind == "vm.state_changed" && ev.Subject == v.UUID {
				if ev.Meta["new_state"] == "running" {
					sawTransition = true
				}
			}
		default:
			i = 10
		}
	}
	if !sawTransition {
		t.Errorf("expected vm.state_changed event with new_state=running")
	}
}

func TestAdapter_UnregisterVM(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	v, _ := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID, Name: "n", HostUUID: a.localHostUUID(),
	})
	if err := a.UnregisterVM(v.UUID); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.VMByUUID(v.UUID); ok {
		t.Errorf("vm should be gone after Unregister")
	}
	if err := a.UnregisterVM("nope"); err == nil {
		t.Errorf("unregister of unknown UUID should error")
	}
}

func TestAdapter_RenameVMInventory(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	v, _ := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID, Name: "old", HostUUID: a.localHostUUID(),
	})
	if err := a.RenameVMInventory(v.UUID, "new"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.VMByName(p.UUID, "old"); ok {
		t.Errorf("old name still resolves")
	}
	if got, ok := a.VMByName(p.UUID, "new"); !ok || got.UUID != v.UUID {
		t.Errorf("new name doesn't resolve")
	}
}
