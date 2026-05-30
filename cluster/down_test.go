package cluster

import (
	"strings"
	"testing"

	"github.com/openweft/weft/infra"
)

func TestBuildDownPlan_SingleHost(t *testing.T) {
	c := &Cluster{
		Name:    "dev",
		Overlay: &Overlay{Subnet: "10.9.0.0/24"},
		Hosts:   []Host{{ID: "h1", Address: "127.0.0.1", DC: "h1"}},
	}
	p, err := BuildDownPlan(c, []*infra.Plan{etcdPlan(), dexPlan()}, DownOptions{})
	if err != nil {
		t.Fatalf("BuildDownPlan: %v", err)
	}
	// One StopReplica per service (single-node collapses etcd to 1).
	sr := actionsOf(p, StopReplica)
	if len(sr) != 2 {
		t.Fatalf("stop-replica = %d, want 2 (etcd + dex)", len(sr))
	}
	// Reverse topo order — dex (depends on etcd) stops before etcd.
	if sr[0].Service != "dex" || sr[1].Service != "etcd" {
		t.Errorf("stop order = [%s,%s], want [dex,etcd] (reverse topo)", sr[0].Service, sr[1].Service)
	}
	for _, a := range sr {
		if a.Host != "h1" {
			t.Errorf("%s/%d stopped on %s, want h1", a.Service, a.Replica, a.Host)
		}
	}
	if sa := actionsOf(p, StopAgent); len(sa) != 1 || sa[0].Host != "h1" {
		t.Errorf("stop-agent = %v, want one on h1", sa)
	}
	if tm := actionsOf(p, TeardownMesh); len(tm) != 1 || tm[0].Host != "h1" {
		t.Errorf("teardown-mesh = %v, want one on h1", tm)
	}
	if pu := actionsOf(p, Purge); len(pu) != 0 {
		t.Errorf("purge = %v, want none without --purge", pu)
	}
}

func TestBuildDownPlan_ThreeDC(t *testing.T) {
	p, err := BuildDownPlan(threeHostCluster(), []*infra.Plan{etcdPlan(), dexPlan()}, DownOptions{})
	if err != nil {
		t.Fatalf("BuildDownPlan: %v", err)
	}
	// etcd → one replica per host, dex → one on h1; total = 4 stop-replicas.
	sr := actionsOf(p, StopReplica)
	if len(sr) != 4 {
		t.Fatalf("stop-replica count = %d, want 4 (3 etcd + 1 dex)", len(sr))
	}
	// dex (top of reverse order) first.
	if sr[0].Service != "dex" {
		t.Errorf("first stop = %s, want dex (reverse topo)", sr[0].Service)
	}
	// Subsequent stops are all etcd.
	gotHosts := map[string]bool{}
	for _, a := range sr[1:] {
		if a.Service != "etcd" {
			t.Errorf("expected etcd in tail, got %s/%d", a.Service, a.Replica)
		}
		gotHosts[a.Host] = true
	}
	for _, h := range []string{"h1", "h2", "h3"} {
		if !gotHosts[h] {
			t.Errorf("etcd missing from host %s; got %v", h, gotHosts)
		}
	}
	// StopAgent + TeardownMesh on each host.
	if sa := actionsOf(p, StopAgent); len(sa) != 3 {
		t.Errorf("stop-agent count = %d, want 3", len(sa))
	}
	if tm := actionsOf(p, TeardownMesh); len(tm) != 3 {
		t.Errorf("teardown-mesh count = %d, want 3", len(tm))
	}
	// Global order: all StopReplica, then StopAgent, then TeardownMesh.
	saw := struct{ replica, agent, mesh int }{-1, -1, -1}
	for i, a := range p.Actions {
		switch a.Kind {
		case StopReplica:
			saw.replica = i
		case StopAgent:
			if saw.agent < 0 {
				saw.agent = i
			}
		case TeardownMesh:
			if saw.mesh < 0 {
				saw.mesh = i
			}
		}
	}
	if !(saw.replica < saw.agent && saw.agent < saw.mesh) {
		t.Errorf("phase order replica<agent<mesh = (%d,%d,%d)", saw.replica, saw.agent, saw.mesh)
	}
}

func TestBuildDownPlan_PurgeAppended(t *testing.T) {
	p, err := BuildDownPlan(threeHostCluster(), []*infra.Plan{etcdPlan(), dexPlan()}, DownOptions{Purge: true})
	if err != nil {
		t.Fatalf("BuildDownPlan: %v", err)
	}
	pu := actionsOf(p, Purge)
	if len(pu) != 3 {
		t.Fatalf("purge count = %d, want 3 (one per host)", len(pu))
	}
	// Purge must come AFTER TeardownMesh, never before — otherwise we'd
	// delete state on a host still holding mesh keys.
	lastMesh, firstPurge := -1, -1
	for i, a := range p.Actions {
		if a.Kind == TeardownMesh {
			lastMesh = i
		}
		if a.Kind == Purge && firstPurge < 0 {
			firstPurge = i
		}
	}
	if firstPurge < lastMesh {
		t.Errorf("purge at %d precedes last teardown-mesh at %d", firstPurge, lastMesh)
	}
}

func TestRenderAction_StopReplica(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{
		Kind: StopReplica, Service: "etcd", Replica: 2, Host: "h2", DC: "dc2",
		Image: "infra-etcd-dc2",
	})
	if host != "h2" {
		t.Errorf("host = %s, want h2", host)
	}
	for _, frag := range []string{"weft microvm rm infra-etcd-dc2", "|| true", "replica 2", "dc=dc2"} {
		if !strings.Contains(cmd, frag) {
			t.Errorf("StopReplica cmd missing %q: %s", frag, cmd)
		}
	}
}

func TestRenderAction_StopAgent(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{Kind: StopAgent, Host: "h2"})
	if host != "h2" {
		t.Errorf("host = %s, want h2", host)
	}
	if !strings.Contains(cmd, "pkill -x weft") || !strings.Contains(cmd, "|| true") {
		t.Errorf("StopAgent cmd = %q, want pkill+tolerant", cmd)
	}
}

func TestRenderAction_TeardownMesh(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{Kind: TeardownMesh, Host: "h3"})
	if host != "h3" {
		t.Errorf("host = %s, want h3", host)
	}
	for _, frag := range []string{"/etc/wireguard/wg0.conf", "wg-quick down wg0", "|| true"} {
		if !strings.Contains(cmd, frag) {
			t.Errorf("TeardownMesh cmd missing %q: %s", frag, cmd)
		}
	}
}

func TestRenderAction_Purge(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{Kind: Purge, Host: "h1"})
	if host != "h1" {
		t.Errorf("host = %s, want h1", host)
	}
	for _, frag := range []string{"$HOME/.weft", "/var/lib/weft", "rm -rf"} {
		if !strings.Contains(cmd, frag) {
			t.Errorf("Purge cmd missing %q: %s", frag, cmd)
		}
	}
}
