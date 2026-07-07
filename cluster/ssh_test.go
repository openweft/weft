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

// TestRenderSSH_DriversEnvPropagated: the drivers block is propagated
// into the systemd unit as Environment= lines so the agent's plugin-
// pull path reads the right registry+version on startup.
func TestRenderSSH_DriversEnvPropagated(t *testing.T) {
	c := threeHostCluster()
	c.Drivers = &Drivers{Registry: "ghcr.io/openweft", Version: "v1.0.0"}
	p, err := Build(c, []*infra.Plan{etcdPlan()}, State{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	hps := RenderSSH(c, p)
	// Every host's unit file carries the driver env.
	for _, hp := range hps {
		var found bool
		for _, s := range hp.Steps {
			if strings.Contains(s, "Environment=WEFT_DRIVER_REGISTRY=ghcr.io/openweft") &&
				strings.Contains(s, "Environment=WEFT_DRIVER_VERSION=v1.0.0") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("host %s unit file missing driver env Environment= lines", hp.Target.HostID)
		}
	}
}

func TestRenderSSH_RolesAndPlacement(t *testing.T) {
	c := threeHostCluster()
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
	if hps[0].Target.HostID != "h1" {
		t.Errorf("first host plan = %s, want seed h1", hps[0].Target.HostID)
	}
	// Every host's unit file carries the hypervisor env so the agent
	// picks the right driver plugin without --hypervisor.
	for _, hp := range hps {
		var found bool
		for _, s := range hp.Steps {
			if strings.Contains(s, "Environment=WEFT_HYPERVISOR=qemu") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("host %s unit file missing WEFT_HYPERVISOR env", hp.Target.HostID)
		}
	}
	// Each host installs the systemd unit.
	for _, hp := range hps {
		var found bool
		for _, s := range hp.Steps {
			if strings.Contains(s, "/etc/systemd/system/weft-agent.service") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("host %s missing systemd unit install: %v", hp.Target.HostID, hp.Steps)
		}
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

// TestRenderAction_EnsureHostInstallsSystemd : EnsureHost installs +
// enables the weft-agent systemd unit so the daemon survives crashes
// and reboots without an operator nohup chain. Replaced the legacy
// "pgrep guard + nohup &" detach trick — systemd Restart=always +
// the unit's idempotent enable handle the same two concerns properly.
func TestRenderAction_EnsureHostInstallsSystemd(t *testing.T) {
	c := threeHostCluster()
	c.Hosts[0].Hypervisor = "qemu"
	c.Hosts[1].Hypervisor = "qemu"
	for _, host := range []string{c.Hosts[0].ID, c.Hosts[1].ID} {
		_, cmd := renderAction(c, Action{Kind: EnsureHost, Host: host})
		for _, frag := range []string{
			"sudo tee /etc/systemd/system/weft-agent.service",
			"ExecStart=/usr/local/bin/weft agent",
			"Restart=always",
			"sudo systemctl daemon-reload",
			"sudo systemctl enable --now weft-agent.service",
		} {
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

// TestRenderAction_EnsureInitrdPullsOCIArtifact: EnsureInitrd actions render
// as `weft microvm pull-pod-initrd <ref>` so the shared cpio.gz lands in
// $XDG_DATA_HOME/weft-microvm/pod-initrd before any PlaceReplica's pod-mode
// boot path needs it (closes the manual scp dance from
// feedback_initrd_with_crun_workflow).
func TestRenderAction_EnsureInitrdPullsOCIArtifact(t *testing.T) {
	c := threeHostCluster()
	host, cmd := renderAction(c, Action{
		Kind: EnsureInitrd, Host: c.Hosts[0].ID,
		Image: "ghcr.io/openweft/weft-microvm-pod-initrd:v0.2.1",
	})
	if host != c.Hosts[0].ID {
		t.Errorf("EnsureInitrd rendered host=%q, want %q", host, c.Hosts[0].ID)
	}
	if !strings.Contains(cmd, "weft microvm pull-pod-initrd ghcr.io/openweft/weft-microvm-pod-initrd:v0.2.1") {
		t.Errorf("EnsureInitrd cmd missing pull-pod-initrd: %s", cmd)
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

// TestRenderAction_EnsureHostPlacementEnv : per-host AZ + Rack from
// cluster.hcl land as Environment= lines in the rendered unit file,
// so the agent's selfRegisterHost path picks them up. Replaces the
// legacy --az / --rack CLI flags that the systemd switch retired.
func TestRenderAction_EnsureHostPlacementEnv(t *testing.T) {
	c := threeHostCluster()
	_, cmd := renderAction(c, Action{Kind: EnsureHost, Host: c.Hosts[1].ID})
	for _, frag := range []string{
		"Environment=WEFT_AZ=" + c.Hosts[1].DC,
	} {
		if !strings.Contains(cmd, frag) {
			t.Errorf("EnsureHost cmd missing %q: %s", frag, cmd)
		}
	}
}

// TestRenderAction_PushAgentConfigHeredoc: PushAgentConfig must produce a
// `sudo install -d /etc/weft && sudo tee ... <<'__WEFT_HCL_EOF__'` heredoc
// carrying the rendered HCL, with the marker terminating the body on its
// own line. The marker shape is the contract — its uniqueness is what
// guarantees the heredoc can't be closed by a stray line in the HCL.
func TestRenderAction_PushAgentConfigHeredoc(t *testing.T) {
	c := threeHostCluster()
	payload := "socket = \"/var/run/weft/weft.sock\"\n"
	host, cmd := renderAction(c, Action{
		Kind:   PushAgentConfig,
		Host:   c.Hosts[1].ID,
		Config: payload,
	})
	if host != c.Hosts[1].ID {
		t.Errorf("PushAgentConfig host = %q, want %q", host, c.Hosts[1].ID)
	}
	for _, frag := range []string{
		"sudo install -d /etc/weft",
		"sudo tee /etc/weft/weft.hcl",
		"<<'__WEFT_HCL_EOF__'",
		payload,
	} {
		if !strings.Contains(cmd, frag) {
			t.Errorf("PushAgentConfig cmd missing %q:\n%s", frag, cmd)
		}
	}
	// Marker must close the heredoc at the start of a line at the end —
	// otherwise the shell never sees EOF and the rest of the action stream
	// gets eaten as input.
	if !strings.HasSuffix(cmd, "\n__WEFT_HCL_EOF__") {
		t.Errorf("PushAgentConfig cmd doesn't end with newline+marker:\n%s", cmd)
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
