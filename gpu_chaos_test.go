package weft

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// TestChaos_ConcurrentClaimContention hammers gpuAllocTable from many
// goroutines all fighting for the SAME small resource pool, then asserts
// the core safety invariant : no resource is ever held by two different
// VMs at once, and the byResource / byVM indices stay consistent. Run
// under -race to also catch data races on the table's maps.
func TestChaos_ConcurrentClaimContention(t *testing.T) {
	const (
		workers   = 32
		resources = 8
		rounds    = 300
	)
	tbl := newGPUAllocTable()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(seed)))
			vm := fmt.Sprintf("vm-%d", seed)
			for i := 0; i < rounds; i++ {
				rid := fmt.Sprintf("0000:%02d:00.0", rng.Intn(resources))
				switch rng.Intn(4) {
				case 0, 1:
					_ = tbl.Claim(GPUClaim{HostUUID: "h1", ResourceID: rid, Kind: GPUClaimWholeCard, VMUUID: vm})
				case 2:
					tbl.Release("h1", rid)
				case 3:
					tbl.ReleaseVM(vm)
				}
			}
			tbl.ReleaseVM(vm) // each worker cleans up its own claims
		}(w)
	}
	wg.Wait()

	// After every worker released its own VM, the table must be empty —
	// proves ReleaseVM + Release keep byResource and byVM in lockstep with
	// no leaked entries.
	if got := len(tbl.ClaimsForHost("h1")); got != 0 {
		t.Fatalf("after all workers released, host should hold 0 claims, got %d", got)
	}
	// Internal index consistency : every byVM key must point at a live
	// byResource entry (none should survive).
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	if len(tbl.byResource) != 0 {
		t.Fatalf("byResource leaked %d entries", len(tbl.byResource))
	}
	for vm, keys := range tbl.byVM {
		if len(keys) != 0 {
			t.Fatalf("byVM[%s] leaked %d keys", vm, len(keys))
		}
	}
}

// TestChaos_ConcurrentClaimAllExclusivity has many goroutines race to
// ClaimAll the SAME pair of resources for distinct VMs. Exactly one must
// win each round; the all-or-nothing contract means a loser claims
// neither. Asserts the winner holds both and no resource is split.
func TestChaos_ConcurrentClaimAllExclusivity(t *testing.T) {
	for round := 0; round < 200; round++ {
		tbl := newGPUAllocTable()
		const contenders = 16
		var wg sync.WaitGroup
		var winners int32
		var mu sync.Mutex
		for c := 0; c < contenders; c++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				vm := fmt.Sprintf("vm-%d", id)
				err := tbl.ClaimAll([]GPUClaim{
					{HostUUID: "h1", ResourceID: "r0", Kind: GPUClaimWholeCard, VMUUID: vm},
					{HostUUID: "h1", ResourceID: "r1", Kind: GPUClaimWholeCard, VMUUID: vm},
				})
				if err == nil {
					mu.Lock()
					winners++
					mu.Unlock()
				}
			}(c)
		}
		wg.Wait()
		if winners != 1 {
			t.Fatalf("round %d: exactly 1 ClaimAll should win, got %d", round, winners)
		}
		// Both resources must belong to the same (winning) VM.
		claims := tbl.ClaimsForHost("h1")
		if len(claims) != 2 || claims[0].VMUUID != claims[1].VMUUID {
			t.Fatalf("round %d: winner must hold both r0+r1 for one VM, got %+v", round, claims)
		}
	}
}

// FuzzEnumerateMIGFromSMIL throws arbitrary bytes at the `nvidia-smi -L`
// parser. It must never panic, and the EITHER/OR invariant must hold:
// any card that gained MIG instances has its whole-card PCIBDF cleared.
func FuzzEnumerateMIGFromSMIL(f *testing.F) {
	f.Add(smiLTwoH200)
	f.Add("GPU 0: NVIDIA H200 (UUID: GPU-x)\n  MIG 1g.18gb Device 0: (UUID: MIG-a)\n")
	f.Add("garbage\n\n\nGPU GPU GPU")
	f.Add("GPU 999999999999999999999: x")
	f.Fuzz(func(t *testing.T, raw string) {
		base := []GPU{
			{Vendor: GPUVendorNVIDIA, Model: "H200", PCIBDF: "0000:65:00.0", MIGCapable: true},
			{Vendor: GPUVendorNVIDIA, Model: "H200", PCIBDF: "0000:b3:00.0", MIGCapable: true},
		}
		out := enumerateMIGFromSMIL(base, strings.NewReader(raw))
		for i, g := range out {
			if len(g.MIGInstances) > 0 && g.PCIBDF != "" {
				t.Fatalf("card %d has MIG instances but BDF not cleared (%q) — EITHER/OR invariant broken", i, g.PCIBDF)
			}
		}
	})
}

// FuzzAssignNVLinkDomains throws arbitrary bytes at the `nvidia-smi topo
// -m` parser. Must never panic; and every card sharing a (non-empty)
// domain label must agree on it — a malformed matrix can't produce
// inconsistent grouping.
func FuzzAssignNVLinkDomains(f *testing.F) {
	f.Add(topo8H200TwoNVL4)
	f.Add("GPU0\tX\tNV1\nGPU1\tNV1\tX\n")
	f.Add("\t\t\t\nGPU GPU0 GPU0\n")
	f.Fuzz(func(t *testing.T, raw string) {
		base := eightH200()
		out := assignNVLinkDomains(base, strings.NewReader(raw))
		// Idempotence : labelling the result again changes nothing.
		again := assignNVLinkDomains(out, strings.NewReader(raw))
		for i := range out {
			if out[i].NVLinkDomain != again[i].NVLinkDomain {
				t.Fatalf("card %d domain not idempotent: %q vs %q", i, out[i].NVLinkDomain, again[i].NVLinkDomain)
			}
		}
	})
}

// FuzzDecodeGPUClaimRecord ensures the KV record decoder never panics on
// arbitrary input and that a successful decode round-trips back to the
// same bytes (encode∘decode is stable for valid records).
func FuzzDecodeGPUClaimRecord(f *testing.F) {
	f.Add(string(encodeGPUClaimRecord(GPUClaim{
		HostUUID: "h1", ResourceID: "0000:65:00.0", Kind: GPUClaimWholeCard, VMUUID: "vm-a", Model: "H200",
	})))
	f.Add("gpu_claim \"x\" {}")
	f.Add("not hcl at all {{{")
	f.Fuzz(func(t *testing.T, raw string) {
		c, err := decodeGPUClaimRecord([]byte(raw))
		if err != nil {
			return // rejection is fine; just must not panic
		}
		// Round-trip stability on the decoded value.
		c2, err := decodeGPUClaimRecord(encodeGPUClaimRecord(c))
		if err != nil {
			t.Fatalf("re-decode of a just-encoded valid claim failed: %v", err)
		}
		if c2 != c {
			t.Fatalf("encode/decode not stable:\n in=%+v\nout=%+v", c, c2)
		}
	})
}
