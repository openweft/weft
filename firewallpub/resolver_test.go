package firewallpub

import (
	"sort"
	"testing"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft-microvm-init/pkg/pod"
)

// fakeSnap is a hand-rolled Snapshot driven by literal slices/maps.
type fakeSnap struct {
	portsByVM      map[string][]weft.Port
	portsByNetwork map[string][]weft.Port
	networks       map[string]weft.Network
	sgs            map[string]weft.SecurityGroup
	projNets       map[string][]string // projectUUID → []networkUUID
}

func (f *fakeSnap) ListPortsForVM(uuid string) []weft.Port      { return f.portsByVM[uuid] }
func (f *fakeSnap) ListPortsForNetwork(uuid string) []weft.Port { return f.portsByNetwork[uuid] }
func (f *fakeSnap) NetworkByUUID(uuid string) (weft.Network, bool) {
	n, ok := f.networks[uuid]
	return n, ok
}
func (f *fakeSnap) SecurityGroupByUUID(uuid string) (weft.SecurityGroup, bool) {
	g, ok := f.sgs[uuid]
	return g, ok
}
func (f *fakeSnap) ListNetworkUUIDsForProject(p string) []string { return f.projNets[p] }

func TestEffectiveFirewall_NoPorts_EmptyRules(t *testing.T) {
	snap := &fakeSnap{}
	fw := EffectiveFirewall(snap, "vm-1")
	if len(fw.Rules) != 0 {
		t.Errorf("expected no rules, got %d", len(fw.Rules))
	}
}

func TestEffectiveFirewall_PortOverrideSGs(t *testing.T) {
	snap := &fakeSnap{
		portsByVM: map[string][]weft.Port{
			"vm-1": {{
				UUID: "p1", VMUUID: "vm-1", NetworkUUID: "net-1",
				ProjectUUID: "proj-1", IP: "10.0.0.5",
				SecurityGroups: []string{"sg-ssh"},
			}},
		},
		sgs: map[string]weft.SecurityGroup{
			"sg-ssh": {
				UUID: "sg-ssh", ProjectUUID: "proj-1", Name: "ssh",
				Rules: []weft.SecurityRule{
					{Direction: weft.SGDirectionIngress, Protocol: weft.SGProtocolTCP, PortMin: 22, PortMax: 22},
				},
			},
		},
	}
	fw := EffectiveFirewall(snap, "vm-1")
	if len(fw.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %+v", fw.Rules)
	}
	if fw.Rules[0] != (pod.FirewallRule{Direction: "ingress", Protocol: "tcp", PortMin: 22, PortMax: 22}) {
		t.Errorf("rule = %+v", fw.Rules[0])
	}
	if err := fw.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestEffectiveFirewall_InheritsNetworkDefaults(t *testing.T) {
	// Port carries no SG list — must inherit from Network.DefaultSecurityGroups.
	snap := &fakeSnap{
		portsByVM: map[string][]weft.Port{
			"vm-1": {{
				UUID: "p1", VMUUID: "vm-1", NetworkUUID: "net-1",
				ProjectUUID: "proj-1", IP: "10.0.0.5",
			}},
		},
		networks: map[string]weft.Network{
			"net-1": {UUID: "net-1", ProjectUUID: "proj-1",
				DefaultSecurityGroups: []string{"sg-base"}},
		},
		sgs: map[string]weft.SecurityGroup{
			"sg-base": {UUID: "sg-base", ProjectUUID: "proj-1",
				Rules: []weft.SecurityRule{
					{Direction: weft.SGDirectionEgress, Protocol: weft.SGProtocolAny},
				}},
		},
	}
	fw := EffectiveFirewall(snap, "vm-1")
	if len(fw.Rules) != 1 || fw.Rules[0].Direction != "egress" {
		t.Errorf("expected 1 egress rule from network defaults, got %+v", fw.Rules)
	}
	if fw.Rules[0].Protocol != "" {
		t.Errorf("SGProtocolAny should translate to empty Protocol, got %q", fw.Rules[0].Protocol)
	}
}

func TestEffectiveFirewall_RemoteGroupExpandsToMemberCIDRs(t *testing.T) {
	snap := &fakeSnap{
		portsByVM: map[string][]weft.Port{
			"vm-1": {{
				UUID: "p1", VMUUID: "vm-1", NetworkUUID: "net-1",
				ProjectUUID: "proj-1", IP: "10.0.0.5",
				SecurityGroups: []string{"sg-app"},
			}},
		},
		portsByNetwork: map[string][]weft.Port{
			"net-1": {
				// VM-1's own port (also in db SG).
				{UUID: "p1", IP: "10.0.0.5", VMUUID: "vm-1", NetworkUUID: "net-1",
					SecurityGroups: []string{"sg-app"}},
				// Two database servers in sg-db.
				{UUID: "p2", IP: "10.0.0.10", VMUUID: "vm-db1", NetworkUUID: "net-1",
					SecurityGroups: []string{"sg-db"}},
				{UUID: "p3", IP: "10.0.0.11", VMUUID: "vm-db2", NetworkUUID: "net-1",
					SecurityGroups: []string{"sg-db"}},
				// Unrelated port — must not appear.
				{UUID: "p4", IP: "10.0.0.20", VMUUID: "vm-x", NetworkUUID: "net-1",
					SecurityGroups: []string{"sg-other"}},
			},
		},
		networks: map[string]weft.Network{
			"net-1": {UUID: "net-1", ProjectUUID: "proj-1"},
		},
		sgs: map[string]weft.SecurityGroup{
			"sg-app": {
				UUID: "sg-app", ProjectUUID: "proj-1",
				Rules: []weft.SecurityRule{
					// Allow egress to ALL members of sg-db on tcp 5432.
					{Direction: weft.SGDirectionEgress, Protocol: weft.SGProtocolTCP,
						PortMin: 5432, PortMax: 5432, RemoteGroup: "sg-db"},
				},
			},
			"sg-db":    {UUID: "sg-db", ProjectUUID: "proj-1"},
			"sg-other": {UUID: "sg-other", ProjectUUID: "proj-1"},
		},
		projNets: map[string][]string{"proj-1": {"net-1"}},
	}
	fw := EffectiveFirewall(snap, "vm-1")

	got := make([]string, 0, len(fw.Rules))
	for _, r := range fw.Rules {
		got = append(got, r.RemoteCIDR)
	}
	sort.Strings(got)
	want := []string{"10.0.0.10/32", "10.0.0.11/32"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("rule[%d].RemoteCIDR = %s, want %s", i, got[i], want[i])
		}
	}
	if err := fw.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestEffectiveFirewall_DedupAcrossPorts(t *testing.T) {
	// Same SG attached to two ports of the same VM → rule must appear once.
	snap := &fakeSnap{
		portsByVM: map[string][]weft.Port{
			"vm-1": {
				{UUID: "p1", VMUUID: "vm-1", NetworkUUID: "net-1", IP: "10.0.0.5",
					SecurityGroups: []string{"sg-ssh"}},
				{UUID: "p2", VMUUID: "vm-1", NetworkUUID: "net-2", IP: "10.1.0.5",
					SecurityGroups: []string{"sg-ssh"}},
			},
		},
		sgs: map[string]weft.SecurityGroup{
			"sg-ssh": {
				Rules: []weft.SecurityRule{
					{Direction: weft.SGDirectionIngress, Protocol: weft.SGProtocolTCP, PortMin: 22, PortMax: 22},
				},
			},
		},
	}
	fw := EffectiveFirewall(snap, "vm-1")
	if len(fw.Rules) != 1 {
		t.Errorf("expected dedup to 1 rule, got %d: %+v", len(fw.Rules), fw.Rules)
	}
}

func TestEffectiveFirewall_IPv6RemoteGroup(t *testing.T) {
	snap := &fakeSnap{
		portsByVM: map[string][]weft.Port{
			"vm-1": {{UUID: "p1", VMUUID: "vm-1", NetworkUUID: "net-1",
				ProjectUUID: "proj-1", IP: "2001:db8::5",
				SecurityGroups: []string{"sg-app"}}},
		},
		portsByNetwork: map[string][]weft.Port{
			"net-1": {{UUID: "p2", VMUUID: "vm-db", NetworkUUID: "net-1",
				IP: "2001:db8::10", SecurityGroups: []string{"sg-db"}}},
		},
		networks: map[string]weft.Network{"net-1": {UUID: "net-1", ProjectUUID: "proj-1"}},
		sgs: map[string]weft.SecurityGroup{
			"sg-app": {UUID: "sg-app", ProjectUUID: "proj-1",
				Rules: []weft.SecurityRule{
					{Direction: weft.SGDirectionEgress, Protocol: weft.SGProtocolTCP,
						PortMin: 5432, PortMax: 5432, RemoteGroup: "sg-db"},
				}},
			"sg-db": {UUID: "sg-db", ProjectUUID: "proj-1"},
		},
		projNets: map[string][]string{"proj-1": {"net-1"}},
	}
	fw := EffectiveFirewall(snap, "vm-1")
	if len(fw.Rules) != 1 || fw.Rules[0].RemoteCIDR != "2001:db8::10/128" {
		t.Errorf("expected 1 v6 /128 rule, got %+v", fw.Rules)
	}
}
