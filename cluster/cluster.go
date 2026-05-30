// Package cluster reads a weft cluster description (cluster.hcl) and plans a
// convergent bring-up of the control plane + infra micro-VMs across 1 host
// (single-node) or 3 hosts (3-DC). It is the model behind `weft up`: pure
// schema + planning, free of any host-access or hypervisor dependency, so the
// topology and reconcile logic are fully unit-testable. Execution (agent
// bring-up, per-host `weft infra` deploy, mesh push) is layered on top.
package cluster

import (
	"fmt"
	"net/netip"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

// doc is the top-level HCL shape: exactly one `cluster` block in practice.
type doc struct {
	Clusters []Cluster `hcl:"cluster,block"`
}

// Cluster is one weft deployment target. The block label is its name.
type Cluster struct {
	Name    string   `hcl:",label"`
	Overlay *Overlay `hcl:"overlay,block"`
	Hosts   []Host   `hcl:"host,block"`
	Infra   *Infra   `hcl:"infra,block"`
	Drivers *Drivers `hcl:"drivers,block"`
	Microvm *Microvm `hcl:"microvm,block"`
}

// Microvm groups cluster-wide microVM-runtime config — today just the OCI
// reference to the shared kernel artifact (produced by openweft/weft-microvm-
// kernel's CI workflow as an ORAS artifact). Nil / empty kernel_ref means
// "operator pre-staged the kernel" (e.g. via `task arm64-microvm` locally on
// each host), so weft up doesn't emit EnsureKernel actions.
type Microvm struct {
	// KernelRef is the OCI artifact reference for the shared microVM kernel,
	// e.g. "ghcr.io/openweft/weft-microvm-kernel:arm64". weft up emits one
	// EnsureKernel action per host that runs any microVM service, which
	// renders as `weft microvm pull-kernel <ref>`.
	KernelRef string `hcl:"kernel_ref,optional"`
}

// Drivers configures where weft obtains its hypervisor driver plugins
// (weft-driver-vz / weft-driver-qemu) on each host: local-first, then OCI pull
// from this registry. It maps to the WEFT_DRIVER_* env weft reads at runtime;
// `weft up` propagates it to each agent's start command. All fields optional —
// unset falls back to weft's built-in GHCR defaults.
type Drivers struct {
	Registry string `hcl:"registry,optional"` // e.g. "ghcr.io/openweft"
	Version  string `hcl:"version,optional"`  // e.g. "v0.3.1" (defaults to "latest")
	VZRef    string `hcl:"vz_ref,optional"`   // full ref override for weft-driver-vz
	QemuRef  string `hcl:"qemu_ref,optional"` // full ref override for weft-driver-qemu
}

// Env renders the driver config as WEFT_DRIVER_* "KEY=VALUE" assignments, in a
// stable order, for prepending to a remote agent-start command. Empty fields
// are omitted. Nil receiver → no env (weft uses its defaults).
func (d *Drivers) Env() []string {
	if d == nil {
		return nil
	}
	var env []string
	if d.Registry != "" {
		env = append(env, "WEFT_DRIVER_REGISTRY="+d.Registry)
	}
	if d.Version != "" {
		env = append(env, "WEFT_DRIVER_VERSION="+d.Version)
	}
	if d.VZRef != "" {
		env = append(env, "WEFT_DRIVER_VZ_REF="+d.VZRef)
	}
	if d.QemuRef != "" {
		env = append(env, "WEFT_DRIVER_QEMU_REF="+d.QemuRef)
	}
	return env
}

// Overlay is the WireGuard overlay the hosts + infra VMs share.
type Overlay struct {
	Subnet string `hcl:"subnet"` // e.g. "10.9.0.0/24"
}

// Host is one hypervisor node. The block label is its id; DC defaults to the
// id when unset. One host → single-node; three hosts → 3-DC cluster.
type Host struct {
	ID         string `hcl:",label"`
	Address    string `hcl:"address"`              // underlay-reachable IP/host
	DC         string `hcl:"dc,optional"`          // availability-zone label
	Hypervisor string `hcl:"hypervisor,optional"`  // "" (auto) | "vz" | "qemu"
	SSH        *SSH   `hcl:"ssh,block"`            // optional; used by the SSH-push access model
}

// SSH carries optional credentials for reaching a host (access-model
// dependent; the planner ignores it).
type SSH struct {
	User string `hcl:"user,optional"`
	Key  string `hcl:"key,optional"`
}

// Infra selects which infra services to bring up. Empty Services means "all
// plans discovered under infra/".
type Infra struct {
	Services []string `hcl:"services,optional"`
}

// Load reads and validates a cluster.hcl.
func Load(path string) (*Cluster, error) {
	var d doc
	if err := hclsimple.DecodeFile(path, nil, &d); err != nil {
		return nil, fmt.Errorf("cluster: decode %s: %w", path, err)
	}
	if len(d.Clusters) != 1 {
		return nil, fmt.Errorf("cluster: %s must declare exactly one `cluster` block (got %d)", path, len(d.Clusters))
	}
	c := &d.Clusters[0]
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate enforces the supported topologies and fills DC defaults.
func (c *Cluster) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("cluster: missing name label")
	}
	switch len(c.Hosts) {
	case 1, 3:
		// single-node or 3-DC quorum — the two supported shapes today.
	default:
		return fmt.Errorf("cluster %q: expected 1 or 3 hosts, got %d", c.Name, len(c.Hosts))
	}
	if c.Overlay == nil || c.Overlay.Subnet == "" {
		return fmt.Errorf("cluster %q: overlay { subnet = … } is required", c.Name)
	}
	if _, err := netip.ParsePrefix(c.Overlay.Subnet); err != nil {
		return fmt.Errorf("cluster %q: overlay subnet %q: %w", c.Name, c.Overlay.Subnet, err)
	}

	seenID := map[string]bool{}
	seenDC := map[string]bool{}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if h.ID == "" {
			return fmt.Errorf("cluster %q: host[%d] missing id label", c.Name, i)
		}
		if seenID[h.ID] {
			return fmt.Errorf("cluster %q: duplicate host id %q", c.Name, h.ID)
		}
		seenID[h.ID] = true
		if h.Address == "" {
			return fmt.Errorf("cluster %q: host %q: address is required", c.Name, h.ID)
		}
		if h.DC == "" {
			h.DC = h.ID // a host is its own DC by default
		}
		if seenDC[h.DC] {
			return fmt.Errorf("cluster %q: two hosts share dc %q (one host per DC)", c.Name, h.DC)
		}
		seenDC[h.DC] = true
		switch h.Hypervisor {
		case "", "vz", "qemu":
		default:
			return fmt.Errorf("cluster %q: host %q: hypervisor %q must be \"\", \"vz\", or \"qemu\"", c.Name, h.ID, h.Hypervisor)
		}
	}
	return nil
}

// IsCluster reports whether this is a multi-host (3-DC) deployment.
func (c *Cluster) IsCluster() bool { return len(c.Hosts) > 1 }
