//go:build darwin

package weft

import (
	"os"
	"testing"
)

// TestLifecycleIntegration_VMRegistryRoutesDelete is the
// end-to-end test of the multi-host dispatch story:
//
//   1. Spin up an Adapter (single-host install).
//   2. Register a fake remote host with a recording driver.
//   3. RegisterVM placed on the fake host.
//   4. Call DeleteVM(name) — the Adapter should route to the
//      fake host's DeleteVM, NOT the local one.
//   5. Verify the inventory entry is removed.
//
// This proves the integration: VM record + dispatch table are
// the joint source of truth, and lifecycle methods consult them
// instead of always hitting the local host.
func TestLifecycleIntegration_VMRegistryRoutesDelete(t *testing.T) {
	a := newAdapterForVMTest(t)

	p, _, err := a.CreateProject("acme")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := a.RegisterHost(RegisterHostSpec{Hostname: "remote-h"})
	fake := &fakeHypervisor{hostUUID: h2.UUID}
	if err := a.RegisterHostHandle(h2.UUID, &HostHandle{Hypervisor: fake}); err != nil {
		t.Fatal(err)
	}

	// Pre-condition: the VM dir exists on disk so findVMByName
	// resolves to it. The dir contents are irrelevant.
	vmDir := a.vmDirIn(p.UUID, "web")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Register the VM with the remote host UUID.
	if _, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID,
		Name:        "web",
		HostUUID:    h2.UUID,
	}); err != nil {
		t.Fatalf("RegisterVM: %v", err)
	}

	// DeleteVM should route to the remote host's DeleteVM.
	if err := a.DeleteVM("web"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}

	if len(fake.deletedAt) != 1 || fake.deletedAt[0] != vmDir {
		t.Errorf("remote DeleteVM not invoked with %q: got %v", vmDir, fake.deletedAt)
	}
	if _, ok := a.VMByName(p.UUID, "web"); ok {
		t.Errorf("VM inventory entry should be removed after DeleteVM")
	}
}

// TestLifecycleIntegration_FallbackToLocalForLegacy confirms
// the backwards-compat path: a VM dir that exists on disk but
// is NOT in the inventory still dispatches to the local host's
// driver. No regression for VMs provisioned before this
// integration landed.
func TestLifecycleIntegration_FallbackToLocalForLegacy(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("legacy-proj")
	vmDir := a.vmDirIn(p.UUID, "old-web")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteVM("old-web"); err != nil {
		t.Errorf("DeleteVM should succeed via local fallback for legacy VM: %v", err)
	}
}

// TestLifecycleIntegration_LocalRegisteredVMDispatchesLocal
// covers the common single-host case: a VM registered with the
// local host UUID still routes correctly through the dispatch
// table (it just happens to land back on `localHypervisor`).
func TestLifecycleIntegration_LocalRegisteredVMDispatchesLocal(t *testing.T) {
	a := newAdapterForVMTest(t)
	p, _, _ := a.CreateProject("p")
	vmDir := a.vmDirIn(p.UUID, "web")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RegisterVM(CreateVMSpec{
		ProjectUUID: p.UUID,
		Name:        "web",
		HostUUID:    a.localHostUUID(),
	}); err != nil {
		t.Fatalf("RegisterVM: %v", err)
	}
	if err := a.DeleteVM("web"); err != nil {
		t.Errorf("DeleteVM (local-registered VM): %v", err)
	}
	if _, ok := a.VMByName(p.UUID, "web"); ok {
		t.Errorf("inventory entry should be gone")
	}
}
