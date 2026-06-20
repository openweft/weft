//go:build darwin

package weft

// gpu_seam_test.go drives the real RegisterMicroVM path end to end (in
// process, fake hypervisor) to prove the claim→driver seam: a microVM
// that requests a GPU gets the card claimed on the local host AND the
// claimed BDF written into the config.json the QEMU driver reads. The
// driver side (config.json → vfio-pci argv) is covered in
// weft-driver-qemu; together they close the loop.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterMicroVM_GPUClaimAndConfig(t *testing.T) {
	a := newAdapterForRegistries(t)
	_ = installFakeLocalHypervisor(t, a)

	// Give the local host a single H200 with a known BDF (detection
	// would do this; here we seed the registry entry directly).
	lh, ok := a.HostByUUID(a.localHostUUID())
	if !ok {
		t.Fatal("local host missing from registry")
	}
	if _, err := a.RegisterHost(RegisterHostSpec{
		UUID: lh.UUID, Hostname: lh.Hostname,
		Hypervisor: lh.Hypervisor, Architecture: lh.Architecture,
		GPUs: []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200", MemoryGiB: 141, MIGCapable: true, PCIBDF: "0000:65:00.0"}},
	}); err != nil {
		t.Fatalf("seed GPU on local host: %v", err)
	}

	p, _, _ := a.CreateProject("p")
	src := t.TempDir()
	iso := filepath.Join(src, "boot.iso")
	_ = os.WriteFile(iso, []byte("iso"), 0o600)
	req := []GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 1}}

	// 1. Register a GPU microVM → claim + config.json.
	if err := a.RegisterMicroVM(p.UUID, "gpu-vm", MicroVMBoot{BootISO: iso}, nil, req); err != nil {
		t.Fatalf("RegisterMicroVM(GPU): %v", err)
	}
	cfg := readPassthroughConfig(t, a.vmDirIn(p.UUID, "gpu-vm"))
	if len(cfg.PCIPassthrough) != 1 || cfg.PCIPassthrough[0] != "0000:65:00.0" {
		t.Fatalf("config.json pci_passthrough = %v, want [0000:65:00.0]", cfg.PCIPassthrough)
	}
	vm, ok := a.VMByName(p.UUID, "gpu-vm")
	if !ok {
		t.Fatal("gpu-vm not in inventory")
	}
	if !a.gpuClaims.IsClaimed(lh.UUID, "0000:65:00.0") {
		t.Fatal("the H200 should be claimed after RegisterMicroVM")
	}

	// 2. A second GPU VM for the only card → exhausted, and the
	//    half-provisioned dir is cleaned up (no orphan).
	if err := a.RegisterMicroVM(p.UUID, "gpu-vm2", MicroVMBoot{BootISO: iso}, nil, req); err == nil {
		t.Fatal("second GPU VM must fail — the only H200 is already claimed")
	}
	if _, err := os.Stat(a.vmDirIn(p.UUID, "gpu-vm2")); !os.IsNotExist(err) {
		t.Error("failed GPU VM dir should be removed")
	}
	if _, ok := a.VMByName(p.UUID, "gpu-vm2"); ok {
		t.Error("failed GPU VM should not leak an inventory entry")
	}

	// 3. Unregister the first VM → claim released → a third VM schedules.
	if err := a.UnregisterVM(vm.UUID); err != nil {
		t.Fatalf("UnregisterVM: %v", err)
	}
	if a.gpuClaims.IsClaimed(lh.UUID, "0000:65:00.0") {
		t.Fatal("claim should be released after UnregisterVM")
	}
	if err := a.RegisterMicroVM(p.UUID, "gpu-vm3", MicroVMBoot{BootISO: iso}, nil, req); err != nil {
		t.Fatalf("after release the card should be schedulable again: %v", err)
	}
}

// TestRegisterMicroVM_GPUClaimReleasedOnLaterError pins the code-review
// find: RegisterVM (+ GPU claim) now happens BEFORE config.json, so an
// error on a LATER step (here a bad share) must release the claim and the
// inventory entry, not just remove the dir — otherwise the card leaks.
func TestRegisterMicroVM_GPUClaimReleasedOnLaterError(t *testing.T) {
	a := newAdapterForRegistries(t)
	_ = installFakeLocalHypervisor(t, a)
	lh, _ := a.HostByUUID(a.localHostUUID())
	if _, err := a.RegisterHost(RegisterHostSpec{
		UUID: lh.UUID, Hostname: lh.Hostname,
		Hypervisor: lh.Hypervisor, Architecture: lh.Architecture,
		GPUs: []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200", MIGCapable: true, PCIBDF: "0000:65:00.0"}},
	}); err != nil {
		t.Fatalf("seed GPU: %v", err)
	}
	p, _, _ := a.CreateProject("p")
	src := t.TempDir()
	iso := filepath.Join(src, "boot.iso")
	_ = os.WriteFile(iso, []byte("iso"), 0o600)
	req := []GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 1}}

	// A share with an empty Path fails validation AFTER the claim is made.
	err := a.RegisterMicroVM(p.UUID, "leaky", MicroVMBoot{BootISO: iso},
		[]MicroVMShare{{Tag: "data"}}, req)
	if err == nil {
		t.Fatal("bad share should fail RegisterMicroVM")
	}
	if a.gpuClaims.IsClaimed(lh.UUID, "0000:65:00.0") {
		t.Fatal("GPU claim leaked: not released on the post-claim error path")
	}
	if _, ok := a.VMByName(p.UUID, "leaky"); ok {
		t.Fatal("inventory entry leaked after the failed register")
	}
	// The card must be schedulable again — a clean register succeeds.
	if err := a.RegisterMicroVM(p.UUID, "ok", MicroVMBoot{BootISO: iso}, nil, req); err != nil {
		t.Fatalf("card should be free after the failed register: %v", err)
	}
}

func TestRegisterMicroVM_NoGPURequestWritesNoPassthrough(t *testing.T) {
	a := newAdapterForRegistries(t)
	_ = installFakeLocalHypervisor(t, a)
	p, _, _ := a.CreateProject("p")
	src := t.TempDir()
	iso := filepath.Join(src, "boot.iso")
	_ = os.WriteFile(iso, []byte("iso"), 0o600)
	if err := a.RegisterMicroVM(p.UUID, "plain", MicroVMBoot{BootISO: iso}, nil, nil); err != nil {
		t.Fatalf("RegisterMicroVM: %v", err)
	}
	cfg := readPassthroughConfig(t, a.vmDirIn(p.UUID, "plain"))
	if len(cfg.PCIPassthrough) != 0 || len(cfg.MIGDevices) != 0 {
		t.Fatalf("no-GPU VM must carry no passthrough, got pci=%v mig=%v", cfg.PCIPassthrough, cfg.MIGDevices)
	}
}

type passthroughConfig struct {
	PCIPassthrough []string `json:"pci_passthrough"`
	MIGDevices     []string `json:"mig_devices"`
}

func readPassthroughConfig(t *testing.T, vmDir string) passthroughConfig {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(vmDir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var c passthroughConfig
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decode config.json: %v", err)
	}
	return c
}
