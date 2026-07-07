package weft

import (
	"strings"
	"testing"
)

// Verbatim-shaped `nvidia-smi -L` output for a 2×H200 host : card 0 is in
// MIG mode (sliced 7× 1g.18gb), card 1 is whole (MIG off → no sub-lines).
const smiLTwoH200 = `GPU 0: NVIDIA H200 (UUID: GPU-5b1c0d8e-1111-2222-3333-444455556666)
  MIG 1g.18gb     Device  0: (UUID: MIG-9c1e0001-aaaa-bbbb-cccc-ddddeeeeffff)
  MIG 1g.18gb     Device  1: (UUID: MIG-9c1e0002-aaaa-bbbb-cccc-ddddeeeeffff)
GPU 1: NVIDIA H200 (UUID: GPU-7d2a0f9b-7777-8888-9999-aaaabbbbcccc)
`

func TestEnumerateMIGFromSMIL_AttachesAndClearsBDF(t *testing.T) {
	base := []GPU{
		{Vendor: GPUVendorNVIDIA, Model: "H200", PCIBDF: "0000:65:00.0", MIGCapable: true},
		{Vendor: GPUVendorNVIDIA, Model: "H200", PCIBDF: "0000:b3:00.0", MIGCapable: true},
	}
	out := enumerateMIGFromSMIL(base, strings.NewReader(smiLTwoH200))

	// Card 0 : two MIG instances, BDF cleared, ParentBDF preserved.
	if len(out[0].MIGInstances) != 2 {
		t.Fatalf("card 0 want 2 MIG instances, got %d", len(out[0].MIGInstances))
	}
	if out[0].PCIBDF != "" {
		t.Errorf("MIG-mode card must have its whole-card BDF cleared, got %q", out[0].PCIBDF)
	}
	mi := out[0].MIGInstances[0]
	if mi.ParentBDF != "0000:65:00.0" {
		t.Errorf("ParentBDF = %q, want original card BDF", mi.ParentBDF)
	}
	if mi.Profile != "1g.18gb" || mi.MemoryGiB != 18 {
		t.Errorf("instance profile/mem = %q/%d, want 1g.18gb/18", mi.Profile, mi.MemoryGiB)
	}
	if mi.UUID != "MIG-9c1e0001-aaaa-bbbb-cccc-ddddeeeeffff" {
		t.Errorf("instance UUID = %q", mi.UUID)
	}
	if out[0].MIGInstances[1].UUID == out[0].MIGInstances[0].UUID {
		t.Error("the two instances must have distinct UUIDs")
	}

	// Card 1 : whole card, untouched.
	if len(out[1].MIGInstances) != 0 {
		t.Errorf("card 1 (MIG off) should have no instances, got %d", len(out[1].MIGInstances))
	}
	if out[1].PCIBDF != "0000:b3:00.0" {
		t.Errorf("whole card BDF should be intact, got %q", out[1].PCIBDF)
	}

	// base must not have been mutated.
	if base[0].PCIBDF != "0000:65:00.0" || base[0].MIGInstances != nil {
		t.Error("enumerateMIGFromSMIL mutated the input slice")
	}
}

func TestEnumerateMIGFromSMIL_NoMIG(t *testing.T) {
	base := []GPU{{Vendor: GPUVendorNVIDIA, Model: "RTX-6000-Ada", PCIBDF: "0000:01:00.0"}}
	out := enumerateMIGFromSMIL(base, strings.NewReader(
		"GPU 0: NVIDIA RTX 6000 Ada Generation (UUID: GPU-deadbeef-0000-1111-2222-333344445555)\n"))
	if len(out[0].MIGInstances) != 0 || out[0].PCIBDF != "0000:01:00.0" {
		t.Fatalf("no-MIG card should be untouched: bdf=%q instances=%d", out[0].PCIBDF, len(out[0].MIGInstances))
	}
}

func TestEnumerateMIGFromSMIL_OutOfRangeGPUIndexIgnored(t *testing.T) {
	// A MIG line under "GPU 5" when we only have one card must not panic
	// or attach anywhere.
	base := []GPU{{Vendor: GPUVendorNVIDIA, Model: "H200", PCIBDF: "0000:65:00.0", MIGCapable: true}}
	in := "GPU 5: NVIDIA H200 (UUID: GPU-x)\n  MIG 2g.35gb Device 0: (UUID: MIG-orphan)\n"
	out := enumerateMIGFromSMIL(base, strings.NewReader(in))
	if len(out[0].MIGInstances) != 0 || out[0].PCIBDF != "0000:65:00.0" {
		t.Fatal("MIG lines for an out-of-range GPU index must be ignored")
	}
}

func TestMIGProfileMemGiB(t *testing.T) {
	cases := map[string]int{
		"1g.18gb":    18,
		"2g.35gb":    35,
		"3g.71gb":    71,
		"7g.141gb":   141,
		"1g.18gb+me": 18, // trailing qualifier tolerated
		"garbage":    0,  // no dot
		"1g.gb":      0,  // no digits
	}
	for profile, want := range cases {
		if got := migProfileMemGiB(profile); got != want {
			t.Errorf("migProfileMemGiB(%q) = %d, want %d", profile, got, want)
		}
	}
}

func TestParseSMILMIGLine_Rejects(t *testing.T) {
	if _, _, ok := parseSMILMIGLine("GPU 0: NVIDIA H200 (UUID: GPU-x)"); ok {
		t.Error("a GPU header line must not parse as a MIG line")
	}
	if _, _, ok := parseSMILMIGLine("MIG 1g.18gb Device 0:"); ok {
		t.Error("a MIG line without a UUID must be rejected")
	}
	if _, ok := parseSMILGPUIndex("  MIG 1g.18gb Device 0: (UUID: MIG-x)"); ok {
		t.Error("a MIG sub-line must not parse as a GPU index")
	}
}
