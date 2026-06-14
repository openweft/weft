package floatingipl2

import (
	"testing"

	weft "github.com/openweft/weft"
)

type fakeScope struct {
	vms      map[string][]weft.VM
	fips     []weft.FloatingIP
	networks map[string]weft.Network
}

func (f *fakeScope) ListVMsForHost(h string) []weft.VM        { return f.vms[h] }
func (f *fakeScope) ListFloatingIPs() []weft.FloatingIP       { return f.fips }
func (f *fakeScope) NetworkByUUID(u string) (weft.Network, bool) {
	n, ok := f.networks[u]
	return n, ok
}

func TestComputeLocalL2Mappings_PicksOnlyVLANNetworkLocalActive(t *testing.T) {
	scope := &fakeScope{
		vms: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}, {UUID: "vm-b-uuid", Name: "vm-b"}},
		},
		fips: []weft.FloatingIP{
			// Active, local, VLAN-mode → INCLUDED.
			{Address: "192.168.50.42", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-a", NetworkUUID: "net-vlan"},
			// Active, local, BGP-mode → excluded (BGP path).
			{Address: "203.0.113.10", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-b", NetworkUUID: "net-bgp"},
			// Active, REMOTE → excluded.
			{Address: "192.168.50.43", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-remote", NetworkUUID: "net-vlan"},
			// Available (unmapped) → excluded.
			{Address: "192.168.50.44", Status: weft.FIPStatusAvailable, NetworkUUID: "net-vlan"},
		},
		networks: map[string]weft.Network{
			"net-vlan": {UUID: "net-vlan", ExternalMode: weft.NetworkExternalVLAN, VLAN: 100, ParentInterface: "eth0"},
			"net-bgp":  {UUID: "net-bgp", ExternalMode: weft.NetworkExternalBGP},
		},
	}
	got := ComputeLocalL2Mappings(scope, "host-1")
	if len(got) != 1 {
		t.Fatalf("got %d mappings, want 1 : %+v", len(got), got)
	}
	want := L2Mapping{
		PublicIP: "192.168.50.42", NetworkUUID: "net-vlan",
		VLAN: 100, ParentInterface: "eth0", VMName: "vm-a",
	}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestComputeLocalL2Mappings_DefaultModeBGPSkipped(t *testing.T) {
	// ExternalMode == "" (default → bgp) skips L2.
	scope := &fakeScope{
		vms: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "203.0.113.10", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-a", NetworkUUID: "net-1"},
		},
		networks: map[string]weft.Network{
			"net-1": {UUID: "net-1" /* ExternalMode left empty */},
		},
	}
	if got := ComputeLocalL2Mappings(scope, "host-1"); len(got) != 0 {
		t.Errorf("default mode must skip L2 : got %v", got)
	}
}

func TestComputeLocalL2Mappings_NetworkLookupMiss(t *testing.T) {
	// If the FIP's network is unknown to the scope (race with
	// network deletion), the mapping is silently dropped.
	scope := &fakeScope{
		vms: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "192.168.50.42", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetVM, MappedTo: "vm-a", NetworkUUID: "net-missing"},
		},
		networks: map[string]weft.Network{}, // empty
	}
	if got := ComputeLocalL2Mappings(scope, "host-1"); len(got) != 0 {
		t.Errorf("missing network must skip : got %v", got)
	}
}

func TestComputeLocalL2Mappings_NoLocalVMs(t *testing.T) {
	scope := &fakeScope{}
	if got := ComputeLocalL2Mappings(scope, "host-1"); len(got) != 0 {
		t.Errorf("no local VMs → no mappings : got %v", got)
	}
}

func TestComputeLocalL2Mappings_TargetKindLBSkipped(t *testing.T) {
	scope := &fakeScope{
		vms: map[string][]weft.VM{
			"host-1": {{UUID: "vm-a-uuid", Name: "vm-a"}},
		},
		fips: []weft.FloatingIP{
			{Address: "192.168.50.42", Status: weft.FIPStatusActive,
				TargetKind: weft.FIPTargetLB, MappedTo: "vm-a", NetworkUUID: "net-vlan"},
		},
		networks: map[string]weft.Network{
			"net-vlan": {UUID: "net-vlan", ExternalMode: weft.NetworkExternalVLAN, VLAN: 100, ParentInterface: "eth0"},
		},
	}
	if got := ComputeLocalL2Mappings(scope, "host-1"); len(got) != 0 {
		t.Errorf("LB target kind not wired for L2 in v0 : got %v", got)
	}
}
