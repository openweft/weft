package cluster

import (
	"strings"
	"testing"

	"github.com/openweft/weft/infra"
)

func TestTarget_DefaultsAndSSHBlock(t *testing.T) {
	c := &Cluster{Hosts: []Host{
		{ID: "a", Address: "192.0.2.1"},
		{ID: "b", Address: "192.0.2.2", SSH: &SSH{User: "ops", Key: "/k/id"}},
	}}
	if tg := c.Target(c.Hosts[0]); tg.User != "root" || tg.KeyPath != "" {
		t.Errorf("default target = %+v, want user=root no key", tg)
	}
	if tg := c.Target(c.Hosts[1]); tg.User != "ops" || tg.KeyPath != "/k/id" {
		t.Errorf("ssh-block target = %+v", tg)
	}
}

func TestSeed_IsFirstHost(t *testing.T) {
	c := threeHostCluster()
	if c.Seed().ID != "h1" {
		t.Errorf("seed = %s, want h1 (first host)", c.Seed().ID)
	}
}

func TestDrivers_Env(t *testing.T) {
	if env := (*Drivers)(nil).Env(); env != nil {
		t.Errorf("nil Drivers.Env() = %v, want nil", env)
	}
	d := &Drivers{Registry: "ghcr.io/acme", Version: "v2", QemuRef: "ghcr.io/acme/q:edge"}
	got := strings.Join(d.Env(), " ")
	want := "WEFT_DRIVER_REGISTRY=ghcr.io/acme WEFT_DRIVER_VERSION=v2 WEFT_DRIVER_QEMU_REF=ghcr.io/acme/q:edge"
	if got != want {
		t.Errorf("Env() =\n %q\nwant\n %q", got, want)
	}
}

// TestRenderSSH_DriversEnvPropagated: the drivers block is prepended to the
// agent-start commands so each host pulls the configured plugin image.
func TestRenderSSH_DriversEnvPropagated(t *testing.T) {
	c := threeHostCluster()
	c.Drivers = &Drivers{Registry: "ghcr.io/openweft", Version: "v1.0.0"}
	p, err := Build(c, []*infra.Plan{etcdPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	hps := RenderSSH(c, p)
	// Seed agent-start carries the registry+version env before `weft agent`.
	seedStep := hps[0].Steps[0]
	if !strings.Contains(seedStep, "WEFT_DRIVER_REGISTRY=ghcr.io/openweft WEFT_DRIVER_VERSION=v1.0.0 weft agent --server") {
		t.Errorf("seed step missing driver env prefix: %q", seedStep)
	}
	// A joining host carries it too.
	var joinedWithEnv bool
	for _, hp := range hps[1:] {
		for _, s := range hp.Steps {
			if strings.Contains(s, "WEFT_DRIVER_REGISTRY=ghcr.io/openweft") && strings.Contains(s, "weft agent --client") {
				joinedWithEnv = true
			}
		}
	}
	if !joinedWithEnv {
		t.Error("joining host agent-start missing driver env prefix")
	}
}

func TestRenderSSH_RolesAndPlacement(t *testing.T) {
	c := threeHostCluster()
	// Give each host a hypervisor so the flag shows.
	for i := range c.Hosts {
		c.Hosts[i].Hypervisor = "qemu"
	}
	p, err := Build(c, []*infra.Plan{etcdPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	hps := RenderSSH(c, p)
	if len(hps) != 3 {
		t.Fatalf("host plans = %d, want 3", len(hps))
	}
	// First host plan is the seed and starts the control-plane agent.
	if hps[0].Target.HostID != "h1" {
		t.Errorf("first host plan = %s, want seed h1", hps[0].Target.HostID)
	}
	if !strings.Contains(hps[0].Steps[0], "weft agent --server") {
		t.Errorf("seed first step = %q, want agent --server", hps[0].Steps[0])
	}
	if !strings.Contains(hps[0].Steps[0], "--hypervisor=qemu") {
		t.Errorf("seed step missing hypervisor flag: %q", hps[0].Steps[0])
	}
	// A non-seed host joins the seed via the TCP control-plane.
	var joined bool
	for _, hp := range hps[1:] {
		for _, s := range hp.Steps {
			if strings.Contains(s, "--client --control-plane=tcp:192.0.2.1:") {
				joined = true
			}
		}
	}
	if !joined {
		t.Error("expected a non-seed host to join --control-plane=tcp:192.0.2.1:<port>")
	}
	// Each host deploys its etcd replica.
	for _, hp := range hps {
		found := false
		for _, s := range hp.Steps {
			if strings.Contains(s, "weft infra deploy etcd") {
				found = true
			}
		}
		if !found {
			t.Errorf("host %s missing etcd deploy step: %v", hp.Target.HostID, hp.Steps)
		}
	}
}

// TestRenderAction_AgentDetachedAndIdempotent: the EnsureHost command must
// detach the long-lived agent (nohup + &) so Apply's CombinedOutput returns,
// and gate the launch on a pgrep guard so re-applies don't double-start it.
func TestRenderAction_AgentDetachedAndIdempotent(t *testing.T) {
	c := threeHostCluster()
	c.Hosts[0].Hypervisor = "qemu"
	c.Hosts[1].Hypervisor = "qemu"
	for _, host := range []string{c.Hosts[0].ID, c.Hosts[1].ID} {
		_, cmd := renderAction(c, Action{Kind: EnsureHost, Host: host})
		for _, frag := range []string{"pgrep -x weft", "nohup ", "weft agent ", " &", "/tmp/weft-agent.log"} {
			if !strings.Contains(cmd, frag) {
				t.Errorf("EnsureHost cmd for %s missing %q: %s", host, frag, cmd)
			}
		}
	}
}

// TestRenderAction_EnsureKernelPullsOCIArtifact: EnsureKernel actions render
// as `weft microvm pull-kernel <ref>` so the shared kernel binary lands in
// $XDG_DATA_HOME/weft-microvm/kernel before any EnsureImage / PlaceReplica.
func TestRenderAction_EnsureKernelPullsOCIArtifact(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{Kind: EnsureKernel, Host: c.Hosts[0].ID, Image: "ghcr.io/openweft/weft-microvm-kernel:arm64"})
	if host != c.Hosts[0].ID {
		t.Errorf("EnsureKernel rendered host=%q, want %q", host, c.Hosts[0].ID)
	}
	if !strings.Contains(cmd, "weft microvm pull-kernel ghcr.io/openweft/weft-microvm-kernel:arm64") {
		t.Errorf("EnsureKernel cmd missing pull-kernel: %s", cmd)
	}
}

// TestRenderAction_EnsureImagePullsOCIRootfs: EnsureImage actions render as
// `weft microvm pull <ref>` on the target host so the rootfs lands in the
// local weft-microvm cache before the subsequent PlaceReplica deploys.
func TestRenderAction_EnsureImagePullsOCIRootfs(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{Kind: EnsureImage, Host: c.Hosts[0].ID, Image: "quay.io/coreos/etcd:v3.6.0"})
	if host != c.Hosts[0].ID {
		t.Errorf("EnsureImage rendered host=%q, want %q", host, c.Hosts[0].ID)
	}
	if !strings.Contains(cmd, "weft microvm pull quay.io/coreos/etcd:v3.6.0") {
		t.Errorf("EnsureImage cmd missing pull: %s", cmd)
	}
}

// TestRenderAction_PlaceReplicaUsesUploadedPlan: PlaceReplica must point
// `weft infra deploy --plan` at the per-host uploaded plan.hcl, since the
// source tree isn't present on the remote.
func TestRenderAction_PlaceReplicaUsesUploadedPlan(t *testing.T) {
	c := threeHostCluster()
	_, cmd := renderAction(c, Action{Kind: PlaceReplica, Host: c.Hosts[0].ID, Service: "etcd", Replica: 1, DC: "dc1"})
	for _, frag := range []string{"weft infra deploy etcd", "--plan ", "/infra/etcd/plan.hcl"} {
		if !strings.Contains(cmd, frag) {
			t.Errorf("PlaceReplica cmd missing %q: %s", frag, cmd)
		}
	}
}

// TestRenderAction_CrossHostTCPTransport: the seed exposes a --tcp-listen
// for the dev-mode cross-host control plane, and non-seed hosts dial that
// TCP target via --control-plane=tcp:<seed>:<port>.
func TestRenderAction_CrossHostTCPTransport(t *testing.T) {
	c := threeHostCluster()
	_, seedCmd := renderAction(c, Action{Kind: EnsureHost, Host: c.Hosts[0].ID})
	if !strings.Contains(seedCmd, "--tcp-listen=:"+controlPlanePort) {
		t.Errorf("seed EnsureHost missing --tcp-listen=: %s", seedCmd)
	}
	_, joinCmd := renderAction(c, Action{Kind: EnsureHost, Host: c.Hosts[1].ID})
	wantTarget := "--control-plane=tcp:" + c.Hosts[0].Address + ":" + controlPlanePort
	if !strings.Contains(joinCmd, wantTarget) {
		t.Errorf("join EnsureHost missing %q: %s", wantTarget, joinCmd)
	}
}

func TestRenderAction_CrossHostAnchoredOnSeed(t *testing.T) {
	c := threeHostCluster()
	// mesh-sync and grow-quorum run on the seed and are notes (not exec'd).
	if id, cmd := renderAction(c, Action{Kind: MeshSync, Hosts: []string{"h1", "h2", "h3"}}); id != "h1" || !strings.HasPrefix(cmd, "#") {
		t.Errorf("mesh-sync render = (%s,%q), want seed note", id, cmd)
	}
	if id, cmd := renderAction(c, Action{Kind: GrowQuorum, Service: "etcd", From: 1, To: 3}); id != "h1" || !strings.Contains(cmd, "1→3") {
		t.Errorf("grow render = (%s,%q)", id, cmd)
	}
}
