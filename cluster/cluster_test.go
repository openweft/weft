package cluster

import (
	"os"
	"path/filepath"
	"testing"
)

func writeHCL(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cluster.hcl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_SingleHost(t *testing.T) {
	c, err := Load(writeHCL(t, `
cluster "dev" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address    = "127.0.0.1"
    hypervisor = "qemu"
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.IsCluster() {
		t.Error("1 host should not be a cluster")
	}
	if c.Hosts[0].DC != "h1" {
		t.Errorf("DC default = %q, want host id h1", c.Hosts[0].DC)
	}
}

func TestLoad_ThreeDC(t *testing.T) {
	c, err := Load(writeHCL(t, `
cluster "prod" {
  overlay { subnet = "10.9.0.0/24" }
  host "a" {
    address = "192.0.2.1"
    dc      = "dc1"
  }
  host "b" {
    address = "192.0.2.2"
    dc      = "dc2"
  }
  host "c" {
    address = "192.0.2.3"
    dc      = "dc3"
  }
  infra { services = ["etcd", "nats"] }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.IsCluster() {
		t.Error("3 hosts should be a cluster")
	}
}

func TestLoad_RejectsTwoHosts(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "10.9.0.0/24" }
  host "a" { address = "192.0.2.1" }
  host "b" { address = "192.0.2.2" }
}`))
	if err == nil {
		t.Error("2 hosts must be rejected (1 or 3 only)")
	}
}

func TestLoad_RejectsBadSubnet(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "not-a-cidr" }
  host "h1" { address = "127.0.0.1" }
}`))
	if err == nil {
		t.Error("invalid overlay subnet must be rejected")
	}
}

func TestLoad_RejectsDuplicateDC(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "10.9.0.0/24" }
  host "a" {
    address = "192.0.2.1"
    dc      = "dc1"
  }
  host "b" {
    address = "192.0.2.2"
    dc      = "dc1"
  }
  host "c" {
    address = "192.0.2.3"
    dc      = "dc1"
  }
}`))
	if err == nil {
		t.Error("hosts sharing a DC must be rejected")
	}
}

func TestLoad_MultiDriverHost(t *testing.T) {
	// Apple Silicon host running BOTH drivers — native arm64 via VZ,
	// foreign archs via QEMU/TCG. Canonical cross-arch build host.
	c, err := Load(writeHCL(t, `
cluster "build" {
  overlay { subnet = "10.9.0.0/24" }
  host "mac-1" {
    address = "192.0.2.10"
    os      = "darwin"
    arch    = "arm64"
    driver "vz" {
      arch = ["arm64"]
    }
    driver "qemu" {
      arch = ["amd64", "riscv64", "loongarch64"]
    }
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := &c.Hosts[0]
	kinds := h.DriverKinds()
	if len(kinds) != 2 || kinds[0] != "vz" || kinds[1] != "qemu" {
		t.Errorf("DriverKinds = %v ; want [vz qemu]", kinds)
	}
	cases := []struct {
		drv, arch string
		want      bool
	}{
		{"vz", "arm64", true},
		{"vz", "amd64", false},   // VZ doesn't claim amd64 here
		{"qemu", "amd64", true},
		{"qemu", "riscv64", true},
		{"qemu", "arm64", false}, // QEMU's arch list excludes arm64 ; that's VZ's domain
	}
	for _, c := range cases {
		if got := h.SupportsArch(c.drv, c.arch); got != c.want {
			t.Errorf("SupportsArch(%q, %q) = %v ; want %v", c.drv, c.arch, got, c.want)
		}
	}
}

func TestLoad_LegacyHypervisorStillWorks(t *testing.T) {
	// `hypervisor = "qemu"` (legacy single-driver shortcut) keeps working.
	c, err := Load(writeHCL(t, `
cluster "legacy" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address    = "127.0.0.1"
    hypervisor = "qemu"
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := &c.Hosts[0]
	if got := h.DriverKinds(); len(got) != 1 || got[0] != "qemu" {
		t.Errorf("DriverKinds = %v ; want [qemu]", got)
	}
	if !h.SupportsArch("qemu", "arm64") {
		t.Error("legacy host should support its native arch (default arm64) under its single driver")
	}
	if h.SupportsArch("vz", "arm64") {
		t.Error("legacy qemu host should NOT claim vz coverage")
	}
}

func TestLoad_RejectsHypervisorAndDriverBlocks(t *testing.T) {
	// Combining `hypervisor = …` (legacy) and `driver {…}` blocks (modern)
	// on the same host is ambiguous — Validate must reject.
	_, err := Load(writeHCL(t, `
cluster "ambiguous" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address    = "127.0.0.1"
    hypervisor = "vz"
    driver "vz" { arch = ["arm64"] }
  }
}`))
	if err == nil {
		t.Error("mixing legacy `hypervisor =` with `driver` blocks must be rejected")
	}
}

func TestLoad_RejectsDuplicateDriverKind(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "dup" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address = "127.0.0.1"
    driver "qemu" { arch = ["arm64"] }
    driver "qemu" { arch = ["amd64"] }
  }
}`))
	if err == nil {
		t.Error("two `driver \"qemu\"` blocks on one host must be rejected")
	}
}

func TestLoad_RejectsBadArch(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad-arch" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address = "127.0.0.1"
    driver "qemu" { arch = ["mips"] }
  }
}`))
	if err == nil {
		t.Error("unknown arch must be rejected")
	}
}

// TestLoad_HostLabels_AndControlPlane: cluster.hcl carries `labels = {…}`
// on the host block and a top-level `control_plane { require_properties = [...] }`.
// Both round-trip through hclsimple, populate the right shape, and the
// require_properties syntax check passes Validate.
func TestLoad_HostLabels_AndControlPlane(t *testing.T) {
	c, err := Load(writeHCL(t, `
cluster "live" {
  overlay { subnet = "10.9.0.0/24" }
  control_plane {
    require_properties = ["role=control-plane"]
  }
  host "h1" {
    address = "192.0.2.1"
    dc      = "dc1"
    properties  = { role = "control-plane", storage = "nvme" }
  }
  host "h2" {
    address = "192.0.2.2"
    dc      = "dc2"
    properties  = { role = "control-plane" }
  }
  host "h3" {
    address = "192.0.2.3"
    dc      = "dc3"
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ControlPlane == nil || len(c.ControlPlane.RequireProperties) != 1 ||
		c.ControlPlane.RequireProperties[0] != "role=control-plane" {
		t.Errorf("ControlPlane = %+v ; want require_properties=[role=control-plane]", c.ControlPlane)
	}
	if got := c.Hosts[0].Properties; got["role"] != "control-plane" || got["storage"] != "nvme" {
		t.Errorf("h1 labels = %v ; want role=control-plane storage=nvme", got)
	}
	if got := c.Hosts[2].Properties; len(got) != 0 {
		t.Errorf("h3 labels = %v ; want empty (no labels block)", got)
	}
	// EligibleControlPlaneHosts filters to the two labeled hosts.
	eligible, err := c.EligibleControlPlaneHosts()
	if err != nil {
		t.Fatalf("EligibleControlPlaneHosts: %v", err)
	}
	if len(eligible) != 2 || eligible[0].ID != "h1" || eligible[1].ID != "h2" {
		t.Errorf("eligible = %v ; want [h1 h2]", eligible)
	}
}

// TestLoad_ControlPlane_BadSyntax: an entry missing `=` is rejected at Load.
func TestLoad_ControlPlane_BadSyntax(t *testing.T) {
	_, err := Load(writeHCL(t, `
cluster "bad" {
  overlay { subnet = "10.9.0.0/24" }
  control_plane { require_properties = ["role-control-plane"] }
  host "h1" { address = "127.0.0.1" }
}`))
	if err == nil {
		t.Error("require_properties entry missing `=` must be rejected")
	}
}

// TestLoad_ControlPlane_AbsentMeansAllEligible: without a control_plane
// block EligibleControlPlaneHosts returns the full host slice.
func TestLoad_ControlPlane_AbsentMeansAllEligible(t *testing.T) {
	c, err := Load(writeHCL(t, `
cluster "open" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" { address = "127.0.0.1" }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	eligible, err := c.EligibleControlPlaneHosts()
	if err != nil {
		t.Fatalf("EligibleControlPlaneHosts: %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != "h1" {
		t.Errorf("eligible = %v ; want full host list", eligible)
	}
}

func TestLoad_DriverArchDefaultsToHostArch(t *testing.T) {
	// `driver "qemu" {}` with no arch list defaults to the host's native arch.
	c, err := Load(writeHCL(t, `
cluster "minimal" {
  overlay { subnet = "10.9.0.0/24" }
  host "h1" {
    address = "127.0.0.1"
    arch    = "amd64"
    driver "qemu" {}
  }
}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Hosts[0].Drivers[0].Arches
	if len(got) != 1 || got[0] != "amd64" {
		t.Errorf("driver arches = %v ; want [amd64] (defaulted to host arch)", got)
	}
}
