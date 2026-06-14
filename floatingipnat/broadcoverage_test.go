package floatingipnat

import (
	"sort"
	"testing"

	weft "github.com/openweft/weft"
)

// broadScope is a fakeScope that ALSO implements ListHostUUIDs,
// triggering the production broad-coverage path : NAT is computed
// for every active FIP regardless of which host runs the target VM.
type broadScope struct {
	*fakeScope
	hostUUIDs []string
}

func (b *broadScope) ListHostUUIDs() []string { return b.hostUUIDs }

func TestComputeLocalMappings_BroadCoverage_FIPForRemoteVMInstalled(t *testing.T) {
	scope := &broadScope{
		fakeScope: &fakeScope{
			vmsByHost: map[string][]weft.VM{
				"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}}, // local
				"host-2": {{UUID: "vm-b-uuid", Name: "vm-b"}}, // remote
			},
			fips: []weft.FloatingIP{
				// Active, mapped to a VM on host-2 (REMOTE).
				// With broad coverage, host-1 still gets the NAT.
				{Address: "203.0.113.42", Status: weft.FIPStatusActive,
					TargetKind: weft.FIPTargetVM, MappedTo: "vm-b"},
				// Active, mapped to a local VM.
				{Address: "203.0.113.43", Status: weft.FIPStatusActive,
					TargetKind: weft.FIPTargetVM, MappedTo: "vm-a"},
			},
			portsByVM: map[string][]weft.Port{
				"vm-a-uuid": {{UUID: "p1", IP: "10.0.0.5"}},
				"vm-b-uuid": {{UUID: "p2", IP: "10.0.0.6"}},
			},
		},
		hostUUIDs: []string{"host-1", "host-2"},
	}
	got := ComputeLocalMappings(scope, "host-1")
	if len(got) != 2 {
		t.Fatalf("broad coverage should install both FIPs on host-1, got %d : %+v",
			len(got), got)
	}
	publics := make([]string, len(got))
	for i, m := range got {
		publics[i] = m.PublicIP
	}
	sort.Strings(publics)
	if publics[0] != "203.0.113.42" || publics[1] != "203.0.113.43" {
		t.Errorf("broad-coverage publics = %v, want both", publics)
	}
}

func TestComputeLocalMappings_LocalOnly_FallbackForMinimalScope(t *testing.T) {
	// Same data but the scope does NOT implement ListHostUUIDs ;
	// the fallback path kicks in → only local FIPs surface.
	scope := &fakeScope{
		vmsByHost: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "203.0.113.42", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-remote"},
			{Address: "203.0.113.43", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-a"},
		},
		portsByVM: map[string][]weft.Port{
			"vm-a-uuid": {{UUID: "p1", IP: "10.0.0.5"}},
		},
	}
	got := ComputeLocalMappings(scope, "host-1")
	if len(got) != 1 || got[0].PublicIP != "203.0.113.43" {
		t.Errorf("minimal scope must fall back to local-only, got %+v", got)
	}
}
