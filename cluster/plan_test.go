package cluster

import (
	"strings"
	"testing"

	"github.com/openweft/weft/infra"
)

// infra fixtures: etcd is a 3-replica quorum (3 static IPs), dex a singleton
// that depends on etcd.
func etcdPlan() *infra.Plan {
	return &infra.Plan{
		Service: "etcd",
		Network: &infra.NetworkBlk{Name: "control-plane", StaticIP: []string{"10.255.1.10", "10.255.1.11", "10.255.1.12"}},
	}
}
func dexPlan() *infra.Plan {
	return &infra.Plan{Service: "dex", DependsOn: []string{"etcd"}}
}

func threeHostCluster() *Cluster {
	return &Cluster{
		Name:    "prod",
		Overlay: &Overlay{Subnet: "10.9.0.0/24"},
		Hosts: []Host{
			{ID: "h1", Address: "192.0.2.1", DC: "dc1"},
			{ID: "h2", Address: "192.0.2.2", DC: "dc2"},
			{ID: "h3", Address: "192.0.2.3", DC: "dc3"},
		},
	}
}

func actionsOf(p *Plan, k ActionKind) []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.Kind == k {
			out = append(out, a)
		}
	}
	return out
}

func TestBuild_SingleHost_CollapsesToOneReplica(t *testing.T) {
	c := &Cluster{
		Name:    "dev",
		Overlay: &Overlay{Subnet: "10.9.0.0/24"},
		Hosts:   []Host{{ID: "h1", Address: "127.0.0.1", DC: "h1"}},
	}
	p, err := Build(c, []*infra.Plan{etcdPlan(), dexPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Topology != "single" {
		t.Errorf("topology = %s, want single", p.Topology)
	}
	if eh := actionsOf(p, EnsureHost); len(eh) != 1 || eh[0].Host != "h1" {
		t.Errorf("ensure-host = %v, want [h1]", eh)
	}
	// Single-node: etcd collapses to ONE replica (no quorum), both on h1.
	pr := actionsOf(p, PlaceReplica)
	if len(pr) != 2 {
		t.Fatalf("place-replica count = %d, want 2 (etcd/1, dex/1)", len(pr))
	}
	for _, a := range pr {
		if a.Host != "h1" {
			t.Errorf("%s/%d placed on %s, want h1", a.Service, a.Replica, a.Host)
		}
	}
	if g := actionsOf(p, GrowQuorum); len(g) != 0 {
		t.Errorf("grow-quorum = %v, want none on single-node bootstrap", g)
	}
}

func TestBuild_ThreeDC_OneReplicaPerDC(t *testing.T) {
	p, err := Build(threeHostCluster(), []*infra.Plan{etcdPlan(), dexPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Topology != "cluster" {
		t.Errorf("topology = %s, want cluster", p.Topology)
	}
	if eh := actionsOf(p, EnsureHost); len(eh) != 3 {
		t.Errorf("ensure-host count = %d, want 3", len(eh))
	}
	if ms := actionsOf(p, MeshSync); len(ms) != 1 || len(ms[0].Hosts) != 3 {
		t.Errorf("mesh-sync = %v, want one with 3 members", ms)
	}
	// etcd → one replica per host, in host order; dex → single, on h1.
	want := map[string]map[int]string{
		"etcd": {1: "h1", 2: "h2", 3: "h3"},
		"dex":  {1: "h1"},
	}
	got := map[string]map[int]string{}
	for _, a := range actionsOf(p, PlaceReplica) {
		if got[a.Service] == nil {
			got[a.Service] = map[int]string{}
		}
		got[a.Service][a.Replica] = a.Host
	}
	for svc, reps := range want {
		for r, h := range reps {
			if got[svc][r] != h {
				t.Errorf("%s/%d on %q, want %q", svc, r, got[svc][r], h)
			}
		}
	}
	if len(got["etcd"]) != 3 {
		t.Errorf("etcd replicas placed = %d, want 3", len(got["etcd"]))
	}
	if g := actionsOf(p, GrowQuorum); len(g) != 0 {
		t.Errorf("grow-quorum on fresh bootstrap = %v, want none", g)
	}
}

func TestBuild_ExtendSingleToCluster(t *testing.T) {
	// Current: single host h1 up, etcd+dex each 1 replica on h1.
	cur := State{
		Hosts:  map[string]bool{"h1": true},
		Placed: map[string]map[int]string{"etcd": {1: "h1"}, "dex": {1: "h1"}},
	}
	p, err := Build(threeHostCluster(), []*infra.Plan{etcdPlan(), dexPlan()}, cur)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Only the two NEW hosts get ensured.
	eh := actionsOf(p, EnsureHost)
	if len(eh) != 2 {
		t.Fatalf("ensure-host = %v, want h2+h3 only", eh)
	}
	for _, a := range eh {
		if a.Host == "h1" {
			t.Error("h1 already up — must not re-ensure")
		}
	}
	// etcd gains replicas 2 and 3 (replica 1 already on h1 → skipped); dex
	// stays put (already placed, singleton).
	pr := actionsOf(p, PlaceReplica)
	if len(pr) != 2 {
		t.Fatalf("place-replica = %v, want etcd/2 + etcd/3", pr)
	}
	for _, a := range pr {
		if a.Service != "etcd" || a.Replica == 1 {
			t.Errorf("unexpected placement %s/%d→%s", a.Service, a.Replica, a.Host)
		}
	}
	// etcd quorum grows 1→3; dex (singleton) does not.
	g := actionsOf(p, GrowQuorum)
	if len(g) != 1 || g[0].Service != "etcd" || g[0].From != 1 || g[0].To != 3 {
		t.Errorf("grow-quorum = %v, want etcd 1→3", g)
	}
	if len(actionsOf(p, MeshSync)) != 1 {
		t.Error("expected a mesh-sync after membership grew")
	}
}

// TestBuild_EmitsPushAgentConfigBeforeEnsureHost: when cluster.hcl carries
// an `agent_config { }` block, Build emits one PushAgentConfig per new host
// IMMEDIATELY before that host's EnsureHost, so weft-agent finds
// /etc/weft/weft.hcl populated on startup.
func TestBuild_EmitsPushAgentConfigBeforeEnsureHost(t *testing.T) {
	c := threeHostCluster()
	c.AgentConfig = &AgentConfigBlock{
		Socket: ptr("/var/run/weft/weft.sock"),
		Storage: &AgentStorageBlock{
			Backend: "etcd",
			Etcd: &AgentEtcdBlock{
				Endpoints: []string{"http://10.0.0.11:2379", "http://10.0.0.12:2379", "http://10.0.0.13:2379"},
			},
		},
	}
	p, err := Build(c, []*infra.Plan{etcdPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// One PushAgentConfig per host, three EnsureHost — each push must
	// appear at index N-1 of its EnsureHost (i.e. immediately before).
	pushes := actionsOf(p, PushAgentConfig)
	if len(pushes) != 3 {
		t.Fatalf("push-agent-config count = %d, want 3", len(pushes))
	}
	// Build the index map and check pairing.
	idxOf := func(kind ActionKind, host string) int {
		for i, a := range p.Actions {
			if a.Kind == kind && a.Host == host {
				return i
			}
		}
		return -1
	}
	for _, h := range c.Hosts {
		pi, ei := idxOf(PushAgentConfig, h.ID), idxOf(EnsureHost, h.ID)
		if pi < 0 || ei < 0 || pi+1 != ei {
			t.Errorf("host %s: push idx=%d ensure idx=%d, want push immediately before ensure", h.ID, pi, ei)
		}
	}
	// Rendered content is non-empty and parseable shape — sanity check.
	for _, a := range pushes {
		if a.Config == "" {
			t.Errorf("host %s: empty Config payload", a.Host)
		}
		if !strings.Contains(a.Config, "/var/run/weft/weft.sock") {
			t.Errorf("host %s: rendered config missing socket: %s", a.Host, a.Config)
		}
	}
}

// TestBuild_NoAgentConfig_NoPushAction: with no agent_config block in
// cluster.hcl, Build emits zero PushAgentConfig actions — we don't want to
// overwrite a host's existing /etc/weft/weft.hcl with an empty file.
func TestBuild_NoAgentConfig_NoPushAction(t *testing.T) {
	c := threeHostCluster()
	p, err := Build(c, []*infra.Plan{etcdPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if pushes := actionsOf(p, PushAgentConfig); len(pushes) != 0 {
		t.Errorf("no agent_config declared → want 0 push actions, got %d", len(pushes))
	}
}

// TestBuild_PushAgentConfig_HostOverrideRendered: a per-host override flows
// into the rendered content for that host only.
func TestBuild_PushAgentConfig_HostOverrideRendered(t *testing.T) {
	c := threeHostCluster()
	c.AgentConfig = &AgentConfigBlock{Socket: ptr("/cluster/sock")}
	c.Hosts[1].AgentConfig = &AgentConfigBlock{Socket: ptr("/h2/sock")}
	p, err := Build(c, []*infra.Plan{etcdPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byHost := map[string]string{}
	for _, a := range actionsOf(p, PushAgentConfig) {
		byHost[a.Host] = a.Config
	}
	if !strings.Contains(byHost["h1"], "/cluster/sock") {
		t.Errorf("h1 config = %q, want cluster default /cluster/sock", byHost["h1"])
	}
	if !strings.Contains(byHost["h2"], "/h2/sock") {
		t.Errorf("h2 config = %q, want host override /h2/sock", byHost["h2"])
	}
	if strings.Contains(byHost["h2"], "/cluster/sock") {
		t.Errorf("h2 config leaked cluster default: %q", byHost["h2"])
	}
}

func TestBuild_AlreadyConverged_NoActions(t *testing.T) {
	cur := State{
		Hosts:  map[string]bool{"h1": true, "h2": true, "h3": true},
		Placed: map[string]map[int]string{"etcd": {1: "h1", 2: "h2", 3: "h3"}, "dex": {1: "h1"}},
	}
	p, err := Build(threeHostCluster(), []*infra.Plan{etcdPlan(), dexPlan()}, cur)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(p.Actions) != 0 {
		t.Errorf("converged cluster should yield 0 actions, got %d:\n%s", len(p.Actions), p.String())
	}
}
