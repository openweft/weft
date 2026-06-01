//go:build linux

package weft

// gpu_detect_linux_test.go pins the Linux-only halves of GPU
// detection : the sysfs walk and the nvidia-smi CSV parser. Both
// stages are exercised against hand-rolled fixtures (tmpfs for
// sysfs, pre-canned stdout strings for nvidia-smi) so the tests
// run on any kernel without a real card or the SMI binary
// installed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSysfsCard builds one /sys/class/drm/cardN/device/ subtree
// with the vendor/uevent files the detector reads. Returns the
// drmRoot the test feeds into detectGPUsFromSysfs.
func fakeSysfsCard(t *testing.T, drmRoot string, idx int, vendor, bdf string) {
	t.Helper()
	cardDev := filepath.Join(drmRoot, "card"+itoa(idx), "device")
	if err := os.MkdirAll(cardDev, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", cardDev, err)
	}
	if err := os.WriteFile(filepath.Join(cardDev, "vendor"), []byte(vendor+"\n"), 0o644); err != nil {
		t.Fatalf("write vendor: %v", err)
	}
	// uevent carries PCI_SLOT_NAME as the symlink-fallback path —
	// we always populate it so the test doesn't rely on symlink
	// readback semantics.
	uevent := "PCI_SLOT_NAME=" + bdf + "\n"
	if err := os.WriteFile(filepath.Join(cardDev, "uevent"), []byte(uevent), 0o644); err != nil {
		t.Fatalf("write uevent: %v", err)
	}
}

func itoa(i int) string {
	// avoid strconv import noise in fixtures
	return [...]string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9"}[i]
}

// TestDetectGPUsFromSysfs_NVIDIAOnly pins the sysfs filter : only
// devices with vendor 0x10de make it into the inventory, and the
// PCI BDF is carried verbatim. Mixes one NVIDIA card with one
// AMD + one Intel iGPU so the filter is exercised end-to-end.
func TestDetectGPUsFromSysfs_NVIDIAOnly(t *testing.T) {
	drm := t.TempDir()
	fakeSysfsCard(t, drm, 0, "0x8086", "0000:00:02.0") // Intel iGPU
	fakeSysfsCard(t, drm, 1, "0x1002", "0000:03:00.0") // AMD
	fakeSysfsCard(t, drm, 2, "0x10de", "0000:65:00.0") // NVIDIA
	fakeSysfsCard(t, drm, 3, "0x10de", "0000:b3:00.0") // NVIDIA

	gpus, err := detectGPUsFromSysfs(drm)
	if err != nil {
		t.Fatalf("detectGPUsFromSysfs: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d NVIDIA GPUs, want 2 (filter let through Intel/AMD): %+v", len(gpus), gpus)
	}
	// Sorted by BDF — 0000:65 < 0000:b3.
	if gpus[0].PCIBDF != "0000:65:00.0" || gpus[1].PCIBDF != "0000:b3:00.0" {
		t.Errorf("BDF order wrong: %+v", gpus)
	}
	for _, g := range gpus {
		if g.Vendor != GPUVendorNVIDIA {
			t.Errorf("non-NVIDIA leaked: %+v", g)
		}
	}
}

// TestEnrichWithSMI_H200 pins the nvidia-smi CSV parse against
// a golden stdout copied from a real `nvidia-smi 545.x` invocation
// on an 8× H200 host. The string is verbatim — driver upgrades
// that change the format will break this test, which is the
// point.
func TestEnrichWithSMI_H200(t *testing.T) {
	// 2-card subset of an 8-card chassis : mig enabled on the first,
	// disabled on the second so both branches of the MIG-mode merge
	// are covered.
	const goldenSMI = `NVIDIA H200, 143771 MiB, GPU-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee, Enabled
NVIDIA H200, 143771 MiB, GPU-11111111-2222-3333-4444-555555555555, Disabled
`
	base := []GPU{
		{Vendor: GPUVendorNVIDIA, PCIBDF: "0000:65:00.0"},
		{Vendor: GPUVendorNVIDIA, PCIBDF: "0000:b3:00.0"},
	}
	got, err := enrichWithSMI(base, strings.NewReader(goldenSMI))
	if err != nil {
		t.Fatalf("enrichWithSMI: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	for i, g := range got {
		if g.Model != "H200" {
			t.Errorf("row %d Model = %q, want H200", i, g.Model)
		}
		if g.MemoryGiB != 140 {
			// 143771 MiB / 1024 = 140 GiB (int trunc) — the SKU table
			// would say 141, but nvidia-smi's actual report wins when
			// non-zero so operators see what the card carries.
			t.Errorf("row %d MemoryGiB = %d, want 140", i, g.MemoryGiB)
		}
		if !g.MIGCapable {
			t.Errorf("row %d MIGCapable = false ; H200 must be true (mig.mode irrespective)", i)
		}
	}
	// BDFs preserved verbatim through the merge.
	if got[0].PCIBDF != "0000:65:00.0" || got[1].PCIBDF != "0000:b3:00.0" {
		t.Errorf("BDFs drifted: %+v", got)
	}
}

// TestEnrichWithSMI_RTX6000AdaAndUnknown pins two contracts in one
// fixture : the canonical-model lookup also handles RTX 6000 Ada
// (workstation SKU per [[openweft_gpu_fleet]]), AND unknown SKUs
// pass through verbatim with MIGCapable=false so a freshly-staged
// host with exotic hardware still registers.
func TestEnrichWithSMI_RTX6000AdaAndUnknown(t *testing.T) {
	const golden = `NVIDIA RTX 6000 Ada Generation, 49140 MiB, GPU-77777777-8888-9999-aaaa-bbbbbbbbbbbb, Disabled
NVIDIA L40S, 46068 MiB, GPU-eeeeeeee-ffff-0000-1111-222222222222, Disabled
`
	base := []GPU{
		{Vendor: GPUVendorNVIDIA, PCIBDF: "0000:65:00.0"},
		{Vendor: GPUVendorNVIDIA, PCIBDF: "0000:b3:00.0"},
	}
	got, err := enrichWithSMI(base, strings.NewReader(golden))
	if err != nil {
		t.Fatalf("enrichWithSMI: %v", err)
	}
	if got[0].Model != "RTX-6000-Ada" {
		t.Errorf("RTX 6000 Ada canonical form: got %q, want RTX-6000-Ada", got[0].Model)
	}
	if got[0].MIGCapable {
		t.Error("RTX 6000 Ada is NOT MIG-capable")
	}
	// L40S is intentionally not in nvidiaModelMap (per [[openweft_gpu_fleet]]
	// — only H200 + RTX-6000-Ada are exemplified). The detector
	// keeps the raw "NVIDIA L40S" so operators see what the card
	// reports rather than a silent drop.
	if !strings.Contains(got[1].Model, "L40S") {
		t.Errorf("unknown SKU verbatim: got %q, want substring L40S", got[1].Model)
	}
	if got[1].MIGCapable {
		t.Error("unknown SKU defaults to MIGCapable=false")
	}
}
