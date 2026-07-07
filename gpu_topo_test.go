package weft

import (
	"strings"
	"testing"
)

// Verbatim-shaped `nvidia-smi topo -m` for an 8×H200 2×NVL4 node :
// GPUs 0-3 form one NVLink island, 4-7 another ; cross-island is SYS.
const topo8H200TwoNVL4 = `	GPU0	GPU1	GPU2	GPU3	GPU4	GPU5	GPU6	GPU7	CPU Affinity	NUMA Affinity
GPU0	 X 	NV18	NV18	NV18	SYS	SYS	SYS	SYS	0-23	0
GPU1	NV18	 X 	NV18	NV18	SYS	SYS	SYS	SYS	0-23	0
GPU2	NV18	NV18	 X 	NV18	SYS	SYS	SYS	SYS	0-23	0
GPU3	NV18	NV18	NV18	 X 	SYS	SYS	SYS	SYS	0-23	0
GPU4	SYS	SYS	SYS	SYS	 X 	NV18	NV18	NV18	24-47	1
GPU5	SYS	SYS	SYS	SYS	NV18	 X 	NV18	NV18	24-47	1
GPU6	SYS	SYS	SYS	SYS	NV18	NV18	 X 	NV18	24-47	1
GPU7	SYS	SYS	SYS	SYS	NV18	NV18	NV18	 X 	24-47	1
`

func eightH200() []GPU {
	g := make([]GPU, 8)
	for i := range g {
		g[i] = GPU{Vendor: GPUVendorNVIDIA, Model: "H200", MIGCapable: true}
	}
	return g
}

func TestAssignNVLinkDomains_TwoNVL4(t *testing.T) {
	out := assignNVLinkDomains(eightH200(), strings.NewReader(topo8H200TwoNVL4))
	// 0-3 share one domain, 4-7 another; the two differ.
	d0, d4 := out[0].NVLinkDomain, out[4].NVLinkDomain
	if d0 == "" || d4 == "" {
		t.Fatalf("both islands should be labelled, got %q / %q", d0, d4)
	}
	if d0 == d4 {
		t.Fatalf("the two NVL4 islands must have distinct domains, both %q", d0)
	}
	for i := 0; i < 4; i++ {
		if out[i].NVLinkDomain != d0 {
			t.Errorf("GPU%d domain = %q, want %q", i, out[i].NVLinkDomain, d0)
		}
	}
	for i := 4; i < 8; i++ {
		if out[i].NVLinkDomain != d4 {
			t.Errorf("GPU%d domain = %q, want %q", i, out[i].NVLinkDomain, d4)
		}
	}
	// Labels are min-index based and stable.
	if d0 != "nvl-0" || d4 != "nvl-4" {
		t.Errorf("domain labels = %q / %q, want nvl-0 / nvl-4", d0, d4)
	}
}

func TestAssignNVLinkDomains_NoNVLinkLeavesEmpty(t *testing.T) {
	// Two cards, all-PCIe (SYS) → no island → both domains stay empty.
	topo := "\tGPU0\tGPU1\tCPU Affinity\nGPU0\t X \tSYS\t0-23\nGPU1\tSYS\t X \t0-23\n"
	out := assignNVLinkDomains([]GPU{
		{Vendor: GPUVendorNVIDIA, Model: "H200"},
		{Vendor: GPUVendorNVIDIA, Model: "H200"},
	}, strings.NewReader(topo))
	if out[0].NVLinkDomain != "" || out[1].NVLinkDomain != "" {
		t.Fatalf("PCIe-only cards must have empty domains, got %q / %q",
			out[0].NVLinkDomain, out[1].NVLinkDomain)
	}
}

func TestAssignNVLinkDomains_MalformedRowLabelNoPanic(t *testing.T) {
	// Regression for the fuzz find "GPU-1 NV0": a negative GPU ordinal
	// must be rejected, not used to index adj[-1].
	for _, raw := range []string{"GPU-1 NV0", "GPU-1\tX\tNV1\n", "GPU\tX"} {
		out := assignNVLinkDomains(eightH200(), strings.NewReader(raw))
		for i, g := range out {
			if g.NVLinkDomain != "" {
				t.Errorf("raw %q: GPU%d should stay unlabelled, got %q", raw, i, g.NVLinkDomain)
			}
		}
	}
	if _, ok := parseTopoGPUIndex("GPU-1"); ok {
		t.Error("parseTopoGPUIndex must reject a negative ordinal")
	}
}

func TestAssignNVLinkDomains_NoMatrixIsNoOp(t *testing.T) {
	out := assignNVLinkDomains(eightH200(), strings.NewReader("nvidia-smi: command produced junk\n"))
	for i, g := range out {
		if g.NVLinkDomain != "" {
			t.Errorf("GPU%d should keep empty domain when no matrix parsed, got %q", i, g.NVLinkDomain)
		}
	}
}

// --- same-domain affinity in selection -------------------------------------

func domainHost(uuid string) Host {
	// 2×NVL4 inventory : 4 cards in nvl-a, 4 in nvl-b, BDFs set.
	var gpus []GPU
	add := func(n int, dom, bdfPrefix string) {
		for i := 0; i < n; i++ {
			gpus = append(gpus, GPU{
				Vendor: GPUVendorNVIDIA, Model: "H200", MIGCapable: true,
				PCIBDF: bdfPrefix + string(rune('0'+i)) + "0.0", NVLinkDomain: dom,
			})
		}
	}
	add(4, "nvl-a", "0000:1")
	add(4, "nvl-b", "0000:2")
	return activeHost(uuid, func(h *Host) { h.GPUs = gpus })
}

func TestSelectGPUClaims_SameDomainAffinity(t *testing.T) {
	h := domainHost("dc1")
	none := func(string) bool { return false }

	// count=4 fits inside one island → all 4 claims share a domain.
	claims, ok := selectGPUClaims(
		[]GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 4}},
		h, "vm-a", 0, none)
	if !ok || len(claims) != 4 {
		t.Fatalf("count=4 should fit one NVL4 island, ok=%v n=%d", ok, len(claims))
	}
	dom := domainOfBDF(h, claims[0].ResourceID)
	for _, c := range claims {
		if domainOfBDF(h, c.ResourceID) != dom {
			t.Fatalf("count=4 claims straddle domains: %v", claims)
		}
	}

	// count=5 can't fit in either 4-card island → rejected (no domain mixing).
	if _, ok := selectGPUClaims(
		[]GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 5}},
		h, "vm-b", 0, none); ok {
		t.Fatal("count=5 must be rejected — no single island has 5 free cards")
	}
}

func TestSelectGPUClaims_AffinityRespectsClaims(t *testing.T) {
	h := domainHost("dc1")
	// Claim one card in nvl-a → that island now has only 3 free, so a
	// count=4 request must land entirely in nvl-b.
	claimedA := "0000:100.0" // first card of nvl-a (bdfPrefix "0000:1" + "0" + "0.0")
	claimed := func(id string) bool { return id == claimedA }
	claims, ok := selectGPUClaims(
		[]GPURequest{{Vendor: GPUVendorNVIDIA, Model: "H200", Count: 4}},
		h, "vm-c", 0, claimed)
	if !ok || len(claims) != 4 {
		t.Fatalf("count=4 should still fit the untouched island, ok=%v n=%d", ok, len(claims))
	}
	for _, c := range claims {
		if domainOfBDF(h, c.ResourceID) != "nvl-b" {
			t.Fatalf("with nvl-a partially claimed, all 4 must be nvl-b: %v", claims)
		}
	}
}

func TestChooseWholeCardsByDomain_UnknownTopologyFallback(t *testing.T) {
	// No named domains (empty) → count=2 allowed over the unconstrained pool.
	free := []GPU{
		{PCIBDF: "0000:01:00.0"},
		{PCIBDF: "0000:02:00.0"},
	}
	if _, ok := chooseWholeCardsByDomain(free, 2); !ok {
		t.Fatal("count=2 with unknown topology should fall back to allowed")
	}
	// Named domains present but none big enough → rejected (no mixing).
	mixed := []GPU{
		{PCIBDF: "a", NVLinkDomain: "nvl-0"},
		{PCIBDF: "b", NVLinkDomain: "nvl-1"},
	}
	if _, ok := chooseWholeCardsByDomain(mixed, 2); ok {
		t.Fatal("count=2 across two single-card domains must be rejected")
	}
}

func domainOfBDF(h Host, bdf string) string {
	for _, g := range h.GPUs {
		if g.PCIBDF == bdf {
			return g.NVLinkDomain
		}
	}
	return "?"
}
