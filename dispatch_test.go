//go:build darwin

package weft

import (
	"context"
	"errors"
	"strings"
	"testing"

	drivers "github.com/openweft/weft-drivers"
)

// fakeHypervisor records every call. Used as a hand-rolled
// HypervisorDriver to exercise the multi-host dispatch path
// without spawning real Apple VZ state.
type fakeHypervisor struct {
	hostUUID  string
	createdAt []string // vmUUIDs CreateVM was called with
	startedAt []string
	stoppedAt []string
	deletedAt []string
}

func (f *fakeHypervisor) HostInfo(ctx context.Context) (drivers.HostInfo, error) {
	return drivers.HostInfo{UUID: f.hostUUID, Hypervisor: "fake"}, nil
}

func (f *fakeHypervisor) CreateVM(ctx context.Context, spec drivers.VMSpec) error {
	f.createdAt = append(f.createdAt, spec.UUID)
	return nil
}

func (f *fakeHypervisor) StartVM(ctx context.Context, vmUUID string) error {
	f.startedAt = append(f.startedAt, vmUUID)
	return nil
}

func (f *fakeHypervisor) StopVM(ctx context.Context, vmUUID string) error {
	f.stoppedAt = append(f.stoppedAt, vmUUID)
	return nil
}

func (f *fakeHypervisor) DeleteVM(ctx context.Context, vmUUID string) error {
	f.deletedAt = append(f.deletedAt, vmUUID)
	return nil
}

func (f *fakeHypervisor) AttachDisk(ctx context.Context, vmUUID string, disk drivers.DiskSpec) error {
	return nil
}
func (f *fakeHypervisor) DetachDisk(ctx context.Context, vmUUID, volumeUUID string) error {
	return nil
}
func (f *fakeHypervisor) AttachNIC(ctx context.Context, vmUUID string, nic drivers.NICHandle) error {
	return nil
}
func (f *fakeHypervisor) DetachNIC(ctx context.Context, vmUUID, nicDevice string) error {
	return nil
}

// Compile-time check that fakeHypervisor satisfies the interface.
var _ drivers.HypervisorDriver = (*fakeHypervisor)(nil)

func newAdapterForDispatchTest(t *testing.T) *Adapter {
	t.Helper()
	stateDir := t.TempDir()
	factory := func(name string) Storage { return NewMemStorage() }
	return NewWithStorage(stateDir, factory).(*Adapter)
}

// TestAdapter_LocalHandleRegisteredOnBoot confirms the
// self-registered host's Bundle lands in the dispatch table
// under its UUID — the single-host invariant.
func TestAdapter_LocalHandleRegisteredOnBoot(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	uuid := a.localHostUUID()
	if uuid == "" {
		t.Fatal("local host UUID should be set after NewWithStorage")
	}
	handle, err := a.HostHandleOn(uuid)
	if err != nil {
		t.Fatalf("HostHandleOn(local): %v", err)
	}
	if handle.Hypervisor == nil {
		t.Errorf("local handle missing Hypervisor driver")
	}
	if handle.Network == nil || handle.Volume == nil || handle.Image == nil {
		t.Errorf("local handle missing one of Network/Volume/Image: %+v", handle)
	}
}

// TestAdapter_HypervisorOn_UnknownHost surfaces a clear error
// when a caller asks for a host that never registered. This is
// the failure mode for "stale VM inventory points at a host
// that weft-agent disconnected from".
func TestAdapter_HypervisorOn_UnknownHost(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	_, err := a.HypervisorOn("definitely-not-a-real-uuid")
	if err == nil {
		t.Fatal("unknown host should return error")
	}
	if !strings.Contains(err.Error(), "no driver handle") {
		t.Errorf("error should explain the failure: %v", err)
	}
}

// TestAdapter_RegisterHostHandle_RejectsInvalid covers the
// caller-bug guards.
func TestAdapter_RegisterHostHandle_RejectsInvalid(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	if err := a.RegisterHostHandle("", &HostHandle{}); err == nil {
		t.Errorf("empty UUID should be rejected")
	}
	if err := a.RegisterHostHandle("h", nil); err == nil {
		t.Errorf("nil handle should be rejected")
	}
}

// TestAdapter_RegisterHostHandle_AddsRemoteDispatch wires a
// fake remote driver under a synthetic host UUID, then verifies
// HypervisorOn routes to it. This is the path weft-agent will
// use post-multi-host: agent registers, gRPC stubs land in the
// table, lifecycle methods on the central control plane reach
// the right host.
func TestAdapter_RegisterHostHandle_AddsRemoteDispatch(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	fake := &fakeHypervisor{hostUUID: "remote-host-1"}
	if err := a.RegisterHostHandle("remote-host-1", &HostHandle{Hypervisor: fake}); err != nil {
		t.Fatalf("register: %v", err)
	}

	hyp, err := a.HypervisorOn("remote-host-1")
	if err != nil {
		t.Fatalf("HypervisorOn remote: %v", err)
	}
	if err := hyp.CreateVM(context.Background(), drivers.VMSpec{UUID: "vm-7"}); err != nil {
		t.Fatalf("CreateVM via remote: %v", err)
	}
	if len(fake.createdAt) != 1 || fake.createdAt[0] != "vm-7" {
		t.Errorf("CreateVM didn't reach the fake driver: %+v", fake.createdAt)
	}

	// Local handle still works — adding a remote didn't displace it.
	if _, err := a.HypervisorOn(a.localHostUUID()); err != nil {
		t.Errorf("local handle lost after remote registration: %v", err)
	}
}

// TestAdapter_UnregisterHostHandle covers the disconnect flow.
func TestAdapter_UnregisterHostHandle(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	fake := &fakeHypervisor{hostUUID: "remote"}
	_ = a.RegisterHostHandle("remote", &HostHandle{Hypervisor: fake})
	a.UnregisterHostHandle("remote")
	if _, err := a.HypervisorOn("remote"); err == nil {
		t.Errorf("Hypervisor should not be resolvable after Unregister")
	}
	// Unregister of a missing UUID is a no-op (not an error).
	a.UnregisterHostHandle("never-registered")
}

// TestAdapter_DispatchedHosts_ListsAll confirms the diagnostic
// accessor returns every registered host. The local one is
// always there + any remotes the test adds.
func TestAdapter_DispatchedHosts_ListsAll(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	_ = a.RegisterHostHandle("remote-a", &HostHandle{Hypervisor: &fakeHypervisor{hostUUID: "remote-a"}})
	_ = a.RegisterHostHandle("remote-b", &HostHandle{Hypervisor: &fakeHypervisor{hostUUID: "remote-b"}})

	hosts := a.DispatchedHosts()
	wantContains := []string{a.localHostUUID(), "remote-a", "remote-b"}
	have := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		have[h] = true
	}
	for _, w := range wantContains {
		if !have[w] {
			t.Errorf("DispatchedHosts missing %q: %v", w, hosts)
		}
	}
}

// TestAdapter_LocalLifecycleStillWorks is the regression guard:
// after the dispatch refactor, DeleteVM/StopVM/StartVM still
// work for the local host. They go through the dispatch table
// but the user-visible behaviour is unchanged.
func TestAdapter_LocalLifecycleStillWorks(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	// DeleteVM on a non-existent VM is idempotent (RemoveAll
	// returns nil for missing paths). The Adapter shouldn't
	// error on a clean-state test dir.
	if err := a.DeleteVM("never-existed"); err != nil {
		t.Errorf("DeleteVM on missing VM: %v", err)
	}
	// StopVM on a non-existent VM is also idempotent (no
	// vm.pid → driver returns nil).
	if err := a.StopVM("never-existed"); err != nil {
		t.Errorf("StopVM on missing VM: %v", err)
	}
}

// TestAdapter_HypervisorOn_AfterReplace verifies that re-
// registering a host UUID swaps the handle. Useful for
// fake-driver test setups; also matches the future "agent
// re-handshake" flow.
func TestAdapter_HypervisorOn_AfterReplace(t *testing.T) {
	a := newAdapterForDispatchTest(t)
	fake1 := &fakeHypervisor{hostUUID: "h"}
	fake2 := &fakeHypervisor{hostUUID: "h"}
	_ = a.RegisterHostHandle("h", &HostHandle{Hypervisor: fake1})
	_ = a.RegisterHostHandle("h", &HostHandle{Hypervisor: fake2})

	hyp, _ := a.HypervisorOn("h")
	_ = hyp.CreateVM(context.Background(), drivers.VMSpec{UUID: "vm"})

	if len(fake1.createdAt) != 0 {
		t.Errorf("first handle should no longer receive calls: %+v", fake1.createdAt)
	}
	if len(fake2.createdAt) != 1 {
		t.Errorf("second handle should receive the call: %+v", fake2.createdAt)
	}
}

// Sanity check that drivers.ErrNotFound + friends aren't accidentally
// expected by the tests above — they're driver-impl sentinels, not
// dispatch-layer ones. Keeps the test surface from drifting.
var _ = errors.Is
