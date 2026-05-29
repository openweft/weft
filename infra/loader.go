// Package infra loads HCL plans for cloud-platform infrastructure
// services (etcd, dex, zot, …) and exposes a small Deploy helper
// that hands a plan to vzd's RegisterMicroVM + StartVM path.
//
// Plans live at pkg/openweft/weft/infra/<service>/plan.hcl with the
// shape:
//
//	service "<name>" {
//	  oci_image  = "quay.io/coreos/etcd:v3.6.0"
//	  resources { cpu_count = 1; memory_mib = 1024 }
//	  volumes    = [ { mount = "...", uuid = "...", size_gib = N } ]
//	  network    { name = "control-plane"; static_ip = ["..."] }
//	  cmdline    = "ncl.rootfs=virtiofs:rootfs0 ..."
//	  config_file { path = "/etc/svc.conf"; template = "..." }
//	  depends_on = ["etcd"]
//	  health     { type = "exec"; cmd = "..."; period = "5s" }
//	}
//
// Today's deploy uses oci_image + resources + cmdline; the richer
// fields (volumes/network/config_file/health/depends_on) are
// parsed and held in the Plan struct for the multi-DC roll-out
// that comes in the next phase. Per [[hcl-over-json]] every field
// is HCL, comments and all.
package infra

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

// planDoc is the top-level HCL shape: one or more `service`
// blocks. In practice a plan.hcl carries exactly one; the slice
// shape matches HCL's blocks-not-attributes constraint.
type planDoc struct {
	Services []Plan `hcl:"service,block"`
}

// Plan describes one infrastructure service. The block label
// (`service "<name>" { … }`) is decoded into `Service`.
type Plan struct {
	Service     string         `hcl:",label"`
	Description string         `hcl:"description,optional"`
	OCIImage    string         `hcl:"oci_image"`
	Resources   *ResourcesBlk  `hcl:"resources,block"`
	Volumes     []VolumeRef    `hcl:"volume,block"`
	Network     *NetworkBlk    `hcl:"network,block"`
	Cmdline     string         `hcl:"cmdline,optional"`
	ConfigFile  *ConfigFileBlk `hcl:"config_file,block"`
	DependsOn   []string       `hcl:"depends_on,optional"`
	Health      *HealthBlk     `hcl:"health,block"`
	Placement   *PlacementBlk  `hcl:"placement,block"`

	// Project the service VM lives in. Defaults to "infra"
	// (admin-owned namespace, off-limits to per-user projects).
	Project string `hcl:"project,optional"`
}

// ResourcesBlk is the `resources { … }` sub-block.
type ResourcesBlk struct {
	CPUCount  uint32 `hcl:"cpu_count,optional"`
	MemoryMiB uint64 `hcl:"memory_mib,optional"`
}

// VolumeRef is one entry of the `volumes = [...]` list. Object
// literals decode into this struct via HCL's natural mapping.
type VolumeRef struct {
	Mount   string `hcl:"mount"`
	UUID    string `hcl:"uuid"`
	SizeGiB uint64 `hcl:"size_gib"`
}

// NetworkBlk is the `network { … }` sub-block. Defines which
// virtual network the service VM joins + optional pre-assigned
// IPs (one per replica when the plan describes a 3-DC service).
type NetworkBlk struct {
	Name     string   `hcl:"name"`
	StaticIP []string `hcl:"static_ip,optional"`
}

// ConfigFileBlk carries a template that vzd materialises into the
// guest at deploy time. The path is inside the guest rootfs;
// `template` substitutes per-replica tokens like $PRIVATE_IP /
// $DC during deploy.
type ConfigFileBlk struct {
	Path     string `hcl:"path"`
	Template string `hcl:"template"`
}

// HealthBlk is the readiness probe vzd polls before declaring a
// service Ready. Today only the `exec` shape is implemented.
type HealthBlk struct {
	Type   string `hcl:"type"`
	Cmd    string `hcl:"cmd,optional"`
	Period string `hcl:"period,optional"`
}

// PlacementBlk is the `placement { ... }` sub-block — declarative
// version of weft.PlacementRule. Translated at deploy time into a
// weft.GroupScheduleRequest by the deployer (cmd/weft/infra.go).
// Per [[vzd-placement-rules]].
//
//   count   how many replicas to deploy (default 1)
//   az      cross-replica AZ proximity:   "same" | "different" | ""
//   rack    cross-replica rack proximity: "same" | "different" | ""
//   host    cross-replica host proximity: "same" | "different" | ""
//
// AZ / Rack / Host are independent dimensions of the placement
// hierarchy (AZ ⊃ Rack ⊃ Host). A 3-replica plan can ask "one
// per AZ" (az="different") or, for an intra-DC HA cluster,
// "one per rack inside the same AZ"
// (az="same", rack="different", host="different").
//
// Strings rather than a typed enum so the HCL stays operator-
// readable ; Validate() rejects anything outside the three
// values so a typo errors at decode time, not at schedule time.
type PlacementBlk struct {
	Count int    `hcl:"count,optional"`
	AZ    string `hcl:"az,optional"`
	Rack  string `hcl:"rack,optional"`
	Host  string `hcl:"host,optional"`
}

// Validate enforces the proximity enum + a sane count. Called from
// applyDefaults so an invalid plan errors at LoadPlan time.
func (b *PlacementBlk) Validate() error {
	if b == nil {
		return nil
	}
	if b.Count < 0 {
		return fmt.Errorf("infra: placement.count must be >= 0, got %d", b.Count)
	}
	if err := validatePlacementProximity("az", b.AZ); err != nil {
		return err
	}
	if err := validatePlacementProximity("rack", b.Rack); err != nil {
		return err
	}
	if err := validatePlacementProximity("host", b.Host); err != nil {
		return err
	}
	return nil
}

// validatePlacementProximity refuses any value other than the
// three the scheduler understands. The empty string maps to
// weft.ProximityAny and is the natural default.
func validatePlacementProximity(field, val string) error {
	switch val {
	case "", "same", "different":
		return nil
	}
	return fmt.Errorf("infra: placement.%s must be \"\", \"same\", or \"different\" (got %q)", field, val)
}

// ReplicaCount returns the operator-declared replica count with
// a default of 1 — convenience for the deployer so it doesn't
// have to nil-check the block + zero-check the Count field at
// every call site.
func (p *Plan) ReplicaCount() int {
	if p == nil || p.Placement == nil || p.Placement.Count == 0 {
		return 1
	}
	return p.Placement.Count
}

// LoadPlan reads + decodes a plan file. Returns a *Plan ready for
// Deploy. Missing required fields error out immediately rather
// than failing late at RegisterMicroVM time.
func LoadPlan(planPath string) (*Plan, error) {
	if planPath == "" {
		return nil, errors.New("infra: plan path is required")
	}
	if _, err := os.Stat(planPath); err != nil {
		return nil, fmt.Errorf("infra: stat %s: %w", planPath, err)
	}
	var doc planDoc
	if err := hclsimple.DecodeFile(planPath, nil, &doc); err != nil {
		return nil, fmt.Errorf("infra: decode %s: %w", planPath, err)
	}
	if len(doc.Services) != 1 {
		return nil, fmt.Errorf("infra: plan %s must declare exactly one `service` block (got %d)", planPath, len(doc.Services))
	}
	p := doc.Services[0]
	if err := p.applyDefaults(); err != nil {
		return nil, err
	}
	return &p, nil
}

// applyDefaults fills in CPU=1 / MemoryMiB=1024 / Project="infra"
// and validates required fields.
func (p *Plan) applyDefaults() error {
	if p.Service == "" {
		return errors.New("infra: service block missing label")
	}
	if p.OCIImage == "" {
		return fmt.Errorf("infra: %s: oci_image is required", p.Service)
	}
	if p.Resources == nil {
		p.Resources = &ResourcesBlk{}
	}
	if p.Resources.CPUCount == 0 {
		p.Resources.CPUCount = 1
	}
	if p.Resources.MemoryMiB == 0 {
		p.Resources.MemoryMiB = 1024
	}
	if p.Project == "" {
		p.Project = "infra"
	}
	if err := p.Placement.Validate(); err != nil {
		return fmt.Errorf("infra: %s: %w", p.Service, err)
	}
	return nil
}

// DefaultPlanPath returns the conventional location for a
// service's plan under the vzd module: `<moduleRoot>/infra/<service>/plan.hcl`.
func DefaultPlanPath(moduleRoot, service string) string {
	return filepath.Join(moduleRoot, "infra", service, "plan.hcl")
}

// CPU + MemoryMiB convenience accessors so callers don't have to
// nil-check Resources.
func (p *Plan) CPU() uint32       { return p.Resources.CPUCount }
func (p *Plan) MemoryMiB() uint64 { return p.Resources.MemoryMiB }

// CmdlineForGuest returns the kernel cmdline to hand to vzd's
// RegisterMicroVM. Defaults to the standard ncl rootfs share when
// the plan didn't set one.
func (p *Plan) CmdlineForGuest() string {
	if p.Cmdline != "" {
		return p.Cmdline
	}
	return "ncl.rootfs=virtiofs:rootfs0 console=hvc0"
}

// VMName returns the conventional micro-VM name for a single-
// replica plan : `infra-<service>`. Prefixed with `infra-` to
// keep the namespace disjoint from user workloads. For multi-
// replica plans use VMNameFor(replica).
func (p *Plan) VMName() string {
	return p.VMNameFor(1)
}

// VMNameFor returns the conventional micro-VM name for the i-th
// replica (1-indexed). Single-replica plans (count <= 1) keep
// the legacy `infra-<service>` shape so operators' muscle memory
// holds ; multi-replica plans use `infra-<service>-dc<i>` so
// each replica is a distinct VM with its own `vm.pid`, log file,
// nats.creds, etc., and the operator can `vzd start / stop` them
// independently.
func (p *Plan) VMNameFor(replica int) string {
	base := "infra-" + p.Service
	if p.ReplicaCount() <= 1 {
		return base
	}
	return fmt.Sprintf("%s-dc%d", base, replica)
}

// OCIImageSafe returns the ncl-style sanitised image ref —
// matches refsafe() in nano-container-linux/runner. Used to
// derive the rootfs path under the ncl image cache.
func (p *Plan) OCIImageSafe() string {
	r := strings.NewReplacer("/", "_", ":", "_")
	return r.Replace(p.OCIImage)
}

// DefaultRootfsPath returns where `ncl pull <image>` puts the
// extracted rootfs ($XDG_DATA_HOME/ncl/images/<refsafe>/rootfs).
// The deploy command checks this path exists before calling
// RegisterMicroVM — operator must pre-pull until vzd grows its
// own OCI client.
func (p *Plan) DefaultRootfsPath() string {
	base := nclDataHome()
	return filepath.Join(base, "images", p.OCIImageSafe(), "rootfs")
}

func nclDataHome() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "ncl")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ncl")
}

// DefaultArtefact resolves the path to the standard ncl-init
// boot artefacts (`kernel`, `initrd`) under $XDG_DATA_HOME/ncl.
func DefaultArtefact(name string) string {
	return filepath.Join(nclDataHome(), name)
}

// ListServices enumerates the services that have a plan under
// `<moduleRoot>/infra/<name>/plan.hcl`. Used by the bootstrap
// orchestrator to discover what's available without an explicit
// `--services` flag. Returns names in lexical order so the
// output stays stable across runs.
func ListServices(moduleRoot string) ([]string, error) {
	root := filepath.Join(moduleRoot, "infra")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("infra: list services in %s: %w", root, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "plan.hcl")); err != nil {
			continue // directory without a plan — skip
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// LoadAllPlans reads + decodes the plan files for each service
// name. Returns the plans keyed by service for downstream lookup
// (TopologicalSort, etc.). Fails fast on the first parse error
// so the operator gets a clean diagnostic.
func LoadAllPlans(moduleRoot string, services []string) (map[string]*Plan, error) {
	out := make(map[string]*Plan, len(services))
	for _, s := range services {
		p, err := LoadPlan(DefaultPlanPath(moduleRoot, s))
		if err != nil {
			return nil, err
		}
		if p.Service != s {
			return nil, fmt.Errorf("infra: plan %s declares service %q (expected %q)", DefaultPlanPath(moduleRoot, s), p.Service, s)
		}
		out[s] = p
	}
	return out, nil
}

// TopologicalSort returns the plans in dependency order: every
// plan appears strictly after the plans listed in its DependsOn.
// Independent plans keep their input order (stable sort over the
// service-name lexical ordering) so the output is deterministic.
//
// Errors :
//   - a `depends_on` entry that points to a service not in `plans`
//     (typo / missing plan) — the operator gets the offending name
//     in the error so they can fix the plan.
//   - a cycle (services A → B → A) — the error names a participant
//     so the operator can locate the cycle in the plan files.
func TopologicalSort(plans map[string]*Plan) ([]*Plan, error) {
	// Validate every DependsOn entry has a matching plan first;
	// catching this here keeps the cycle-detector simple.
	for name, p := range plans {
		for _, d := range p.DependsOn {
			if _, ok := plans[d]; !ok {
				return nil, fmt.Errorf("infra: service %q depends on %q which has no plan", name, d)
			}
		}
	}

	// Stable iteration order so re-runs produce the same output
	// when there are no dependency constraints to break ties.
	names := make([]string, 0, len(plans))
	for n := range plans {
		names = append(names, n)
	}
	sort.Strings(names)

	const (
		unvisited = 0
		onStack   = 1 // currently being explored — a back-edge into onStack is a cycle
		done      = 2
	)
	state := make(map[string]int, len(plans))
	out := make([]*Plan, 0, len(plans))

	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case done:
			return nil
		case onStack:
			return fmt.Errorf("infra: dependency cycle involving %q", name)
		}
		state[name] = onStack
		// Sort deps lexically too — same determinism reason as above.
		deps := append([]string(nil), plans[name].DependsOn...)
		sort.Strings(deps)
		for _, d := range deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[name] = done
		out = append(out, plans[name])
		return nil
	}
	for _, n := range names {
		if err := visit(n); err != nil {
			return nil, err
		}
	}
	return out, nil
}
