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
	// AgentConfig is the cluster-wide weft.hcl content `weft up` pushes
	// to each host's /etc/weft/weft.hcl. Per-host `agent_config { }`
	// blocks (Host.AgentConfig) overlay this default on a per-field
	// basis ; see AgentConfigFor.
	AgentConfig *AgentConfigBlock `hcl:"agent_config,block"`
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
//
// Driver selection — two forms, mutually exclusive on the same host :
//
//   - Legacy single-driver : `hypervisor = "vz" | "qemu"` on the host block.
//     The agent at registration time announces this driver with its OS-default
//     architecture set.
//
//   - Multi-driver : zero-or-more `driver "kind" { arch = [...] }` nested
//     blocks. Each `kind` is "vz" | "qemu" ; the `arch` list enumerates the
//     guest architectures this driver instance can run on this host. The
//     scheduler matches a microVM's required arch against the union of arch
//     sets across all the host's drivers, then picks the matching driver.
//
//     The canonical use is an Apple Silicon host running both : native arm64
//     via `driver "vz" { arch = ["arm64"] }` and foreign archs (amd64 / riscv64
//     / loongarch64) via `driver "qemu" { arch = [...] }` — covers
//     cross-architecture builds without leaving the host.
type Host struct {
	ID         string       `hcl:",label"`
	Address    string       `hcl:"address"`             // underlay-reachable IP/host
	OS         string       `hcl:"os,optional"`         // "" (auto) | "linux" | "darwin" — informational, used to pick default driver when neither hypervisor nor driver blocks are set
	Arch       string       `hcl:"arch,optional"`       // "" (auto) | "arm64" | "amd64" | "riscv64" | "loongarch64" — the host's native arch ; informs default driver coverage
	DC         string       `hcl:"dc,optional"`         // availability-zone label (mapped to host registry AZ)
	Rack       string       `hcl:"rack,optional"`       // sub-AZ placement domain (matched by az=different/rack=different/host=different)
	Hypervisor string       `hcl:"hypervisor,optional"` // legacy single-driver shortcut — "" | "vz" | "qemu" ; mutually exclusive with `driver` blocks
	Drivers    []HostDriver `hcl:"driver,block"`        // multi-driver capability list — empty means "use hypervisor legacy field or OS default"
	SSH        *SSH         `hcl:"ssh,block"`           // optional; used by the SSH-push access model
	// AgentConfig is the per-host override of the cluster-level
	// agent_config { } block. Each non-nil field replaces the cluster
	// default ; absent fields fall through. See AgentConfigFor.
	AgentConfig *AgentConfigBlock `hcl:"agent_config,block"`
}

// HostDriver declares one hypervisor-driver instance running on a host, with
// the set of guest architectures it can launch. Block label is the driver
// kind ("vz" | "qemu"). The agent spawns one weft-driver-<kind> subprocess
// per HostDriver entry on this host.
type HostDriver struct {
	Kind   string   `hcl:",label"`        // "vz" | "qemu"
	Arches []string `hcl:"arch,optional"` // "arm64" | "amd64" | "riscv64" | "loongarch64" ; empty defaults to the host's native arch
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
		switch h.OS {
		case "", "linux", "darwin":
		default:
			return fmt.Errorf("cluster %q: host %q: os %q must be \"\", \"linux\", or \"darwin\"", c.Name, h.ID, h.OS)
		}
		if h.Arch != "" && !validArch(h.Arch) {
			return fmt.Errorf("cluster %q: host %q: arch %q must be arm64/amd64/riscv64/loongarch64", c.Name, h.ID, h.Arch)
		}
		// Multi-driver vs legacy single-driver are mutually exclusive.
		if len(h.Drivers) > 0 && h.Hypervisor != "" {
			return fmt.Errorf("cluster %q: host %q: cannot set both `hypervisor = …` (legacy) and one or more `driver` blocks ; pick one form", c.Name, h.ID)
		}
		seenKind := map[string]bool{}
		for di := range h.Drivers {
			d := &h.Drivers[di]
			switch d.Kind {
			case "vz", "qemu":
			default:
				return fmt.Errorf("cluster %q: host %q: driver kind %q must be \"vz\" or \"qemu\"", c.Name, h.ID, d.Kind)
			}
			if seenKind[d.Kind] {
				return fmt.Errorf("cluster %q: host %q: driver %q declared twice", c.Name, h.ID, d.Kind)
			}
			seenKind[d.Kind] = true
			for _, a := range d.Arches {
				if !validArch(a) {
					return fmt.Errorf("cluster %q: host %q: driver %q: arch %q must be arm64/amd64/riscv64/loongarch64", c.Name, h.ID, d.Kind, a)
				}
			}
			// Empty arch list defaults to the host's native arch (or arm64
			// when unspecified). Resolved here so callers see a populated
			// list ; keeps the runtime side free of "implicit default" logic.
			if len(d.Arches) == 0 {
				native := h.Arch
				if native == "" {
					native = "arm64"
				}
				d.Arches = []string{native}
			}
		}
	}
	return nil
}

// validArch reports whether s is one of the four guest architectures weft's
// kernel + driver pipeline ships for.
func validArch(s string) bool {
	switch s {
	case "arm64", "amd64", "riscv64", "loongarch64":
		return true
	}
	return false
}

// DriverKinds returns the set of driver kinds active on this host, in a
// stable order ("vz" before "qemu"). Combines the legacy `hypervisor` field
// with the modern `driver` blocks — exactly one form is non-empty by
// Validate's mutual-exclusion check, so this is unambiguous.
//
// Callers use it to know how many weft-driver-<kind> subprocesses the host's
// weft-agent has to launch.
func (h *Host) DriverKinds() []string {
	if h.Hypervisor != "" {
		return []string{h.Hypervisor}
	}
	var out []string
	for _, k := range []string{"vz", "qemu"} {
		for _, d := range h.Drivers {
			if d.Kind == k {
				out = append(out, k)
			}
		}
	}
	return out
}

// SupportsArch reports whether this host can launch a guest with the given
// architecture under driver kind. Legacy `hypervisor = …` hosts implicitly
// support their declared `arch` (or arm64 when unset). Modern hosts consult
// the matching `HostDriver` block.
func (h *Host) SupportsArch(driverKind, arch string) bool {
	if h.Hypervisor != "" {
		if driverKind != h.Hypervisor {
			return false
		}
		native := h.Arch
		if native == "" {
			native = "arm64"
		}
		return arch == native
	}
	for _, d := range h.Drivers {
		if d.Kind != driverKind {
			continue
		}
		for _, a := range d.Arches {
			if a == arch {
				return true
			}
		}
	}
	return false
}

// IsCluster reports whether this is a multi-host (3-DC) deployment.
func (c *Cluster) IsCluster() bool { return len(c.Hosts) > 1 }
