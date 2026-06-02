// Package pluginstore implements the weft plugin catalogue : a thin
// layer that turns a declarative plugin.hcl manifest into a sequence
// of CreateNetwork / CreateSecurityGroup / CreateVM RPCs against the
// running weft-agent.
//
// A plugin is glue, not a parallel data plane (per the openweft
// pull-model memo) : every resource it creates goes through the
// existing gRPC API so weft-network reconciles the binding the same
// way it would for hand-written input. The framework keeps a single
// instance UUID per plugin in the dedicated _plugins_ registry so a
// re-run with the same inputs is a no-op (idempotency).
//
// Three runner plugins ship in catalogue/{gitlab,github,forgejo}-
// runners-ha/plugin.hcl. They all share the HA-3-DC layout :
// replicas=3, hard anti-affinity across the `az` axis, dedicated
// `runners` network isolated from tenant traffic, egress-only
// security group pointed at the provider's API endpoint.
package pluginstore

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// Manifest is the in-memory representation of a plugin.hcl file. It
// drives Install : create the listed networks, then security groups,
// then `Replicas` micro-VMs per VMSpec, spread across DCs via the
// scheduling rule binding.
//
// HCL field names follow snake_case ; Go field names use the
// idiomatic CamelCase via gohcl struct tags.
type Manifest struct {
	// Name is the catalogue key (e.g. "gitlab-runners-ha"). One
	// directory under catalogue/ per name.
	Name string `hcl:"name,label"`

	// Version pins the manifest schema version so future changes
	// can bump it without silently re-interpreting old fields.
	// The current schema is "v1".
	Version string `hcl:"version"`

	// Kind groups plugins by what they do — used by CLI filters
	// and (later) by the webui catalogue page.
	Kind string `hcl:"kind"`

	// Description is one short sentence shown in `weft plugin list`.
	Description string `hcl:"description"`

	// Layout pins the topology shape. Currently the only
	// supported value is "ha-3dc" — three replicas with hard
	// anti-affinity across the `az` axis.
	Layout string `hcl:"layout,optional"`

	// Inputs are operator-supplied values surfaced as
	// `--input k=v` flags. Required inputs without a default
	// must be passed at install time.
	Inputs []Input `hcl:"input,block"`

	// Networks are created before any VM.
	Networks []NetworkSpec `hcl:"network,block"`

	// SecurityGroups are created before any VM. Each may
	// reference networks created above via `networks = [...]`.
	SecurityGroups []SecurityGroupSpec `hcl:"security_group,block"`

	// VMs declare the per-spec replica count, image, placement,
	// and network bindings. Replicas spread across DCs by feeding
	// a per-VM `scheduling_rule` to the existing scheduler.
	VMs []VMSpec `hcl:"vm,block"`

	// Remain captures any unknown top-level blocks (e.g.
	// future-schema additions another agent introduces, like
	// the proxy_route block JupyterHub uses). The orchestrator
	// ignores Remain — plugins relying on it must declare
	// their own decoder.
	Remain hcl.Body `hcl:",remain"`
}

// Input declares one plugin input parameter.
type Input struct {
	Name     string   `hcl:"name,label"`
	Type     string   `hcl:"type,optional"`     // "string" (default) / "int" / "bool"
	Default  string   `hcl:"default,optional"`  // string-encoded default
	Required bool     `hcl:"required,optional"` // when true and no Default
	Secret   bool     `hcl:"secret,optional"`   // hide from `plugin list`
	Help     string   `hcl:"help,optional"`
	Remain   hcl.Body `hcl:",remain"`
}

// NetworkSpec is one virtual network the plugin creates.
type NetworkSpec struct {
	Name    string   `hcl:"name,label"`
	CIDR    string   `hcl:"cidr"`
	Type    string   `hcl:"type,optional"` // "nat" (default), "bridged", "isolated"
	Gateway string   `hcl:"gateway,optional"`
	DNS     []string `hcl:"dns,optional"`
	Remain  hcl.Body `hcl:",remain"`
}

// SecurityGroupSpec is one project-scoped SG. Rules follow weft's
// existing SecurityRule shape — direction is "ingress" / "egress",
// protocol is "tcp" / "udp" / "icmp" / "any".
type SecurityGroupSpec struct {
	Name        string `hcl:"name,label"`
	Description string `hcl:"description,optional"`
	// Networks lists the network names (within this manifest)
	// that take this SG as their default. The framework resolves
	// the names to UUIDs on install.
	Networks []string `hcl:"networks,optional"`
	Rules    []Rule   `hcl:"rule,block"`
	Remain   hcl.Body `hcl:",remain"`
}

// Rule mirrors weft.SecurityRule for HCL decoding.
type Rule struct {
	Direction  string `hcl:"direction,label"` // "ingress" / "egress"
	Protocol   string `hcl:"protocol"`        // "tcp" / "udp" / "icmp" / "any"
	PortMin    int    `hcl:"port_min,optional"`
	PortMax    int    `hcl:"port_max,optional"`
	RemoteCIDR string `hcl:"remote_cidr,optional"`
	// Description is informational only — kept here so the
	// HCL author can label each rule.
	Description string   `hcl:"description,optional"`
	Remain      hcl.Body `hcl:",remain"`
}

// VMSpec declares one tier in the plugin topology. The framework
// creates N=Replicas VMs from this spec, naming them
// "<plugin-instance>-<spec-name>-<index>".
type VMSpec struct {
	Name     string `hcl:"name,label"`
	Image    string `hcl:"image"` // OCI ref
	Replicas int    `hcl:"replicas,optional"`
	CPU      int    `hcl:"cpu,optional"`
	MemMB    int    `hcl:"mem_mb,optional"`
	DiskGB   int    `hcl:"disk_gb,optional"`

	// Network references one of the manifest's Networks by name.
	// Empty defaults to the project's default network.
	Network string `hcl:"network,optional"`

	// SchedulingRule is a nominal binding (per the openweft
	// nominal-binding memo). Empty defaults to a per-instance
	// rule derived from the plugin name.
	SchedulingRule string `hcl:"scheduling_rule,optional"`

	// Placement is a free-form set of constraints rendered
	// into the scheduling rule on Install — `az = "different"`
	// in the standard 3-DC plugins.
	Placement *Placement `hcl:"placement,block"`

	// EnvFrom lists input names whose value is injected as an
	// environment variable into the runner. The framework
	// stamps them through the per-VM property surface so the
	// in-guest agent picks them up at boot (pull-reconcile).
	EnvFrom []EnvFromInput `hcl:"env_from,block"`

	// Volumes declare per-replica ephemeral disks. Each entry
	// allocates one CreateVolume + AttachVolume pair.
	Volumes []VolumeSpec `hcl:"volume,block"`

	// Count is an optional multiplicity attribute. When set, the
	// installer materialises N copies of this whole VM block,
	// naming them "<base>-0", ..., "<base>-(N-1)" ; each copy still
	// expands to `Replicas` replicas internally. Accepted grammar :
	//   - an integer literal matching `^[1-9]\d*$`  (e.g. "4")
	//   - an input reference matching `^input\.[a-z_][a-z0-9_]*$`
	//     (e.g. "input.replica_count")
	// Empty means "1" : current behaviour, no name suffix added.
	//
	// Count and Replicas compose multiplicatively : a vm with
	// count=2 and replicas=3 yields 6 CreateVM calls.
	Count string `hcl:"count,optional"`

	// Remain absorbs forward-compatible blocks (e.g. JupyterHub's
	// `share`, `secret_volume`, …) so parsing succeeds even when
	// a plugin uses a newer schema this binary doesn't know.
	Remain hcl.Body `hcl:",remain"`
}

// Placement carries the constraints fed to the scheduler.
type Placement struct {
	AZ     string   `hcl:"az,optional"`   // "different" forces 1 per AZ
	Rack   string   `hcl:"rack,optional"` // optional sub-AZ axis
	Host   string   `hcl:"host,optional"` // host UUID hint
	Remain hcl.Body `hcl:",remain"`
}

// EnvFromInput names an Input whose value should land in the
// VM property store under `env.<env_name>`.
type EnvFromInput struct {
	Input   string   `hcl:"input,label"`
	EnvName string   `hcl:"env_name"`
	Remain  hcl.Body `hcl:",remain"`
}

// VolumeSpec declares one persistent disk attached to each replica.
type VolumeSpec struct {
	Name    string `hcl:"name,label"`
	SizeGiB int    `hcl:"size_gib"`
	Format  string `hcl:"format,optional"` // "raw" (default) / "qcow2"
	Mount   string `hcl:"mount,optional"`  // optional guest mount path (forward-compat hint)
	// Count is an optional multiplicity attribute. When set, the
	// installer materialises N copies of this volume per replica,
	// named "<base>-0", ..., "<base>-(N-1)" and (when Mount is set)
	// mounted at "<Mount>-0", ..., "<Mount>-(N-1)". Same grammar as
	// VMSpec.Count : integer literal or `input.<name>`.
	//
	// This is the mechanism that wires e.g. minio-ha's
	// `volumes_per_node` input to the drive layout — one
	// `volume "drive" { count = input.volumes_per_node }` block
	// replaces N hard-pinned drive-* blocks.
	Count  string   `hcl:"count,optional"`
	Remain hcl.Body `hcl:",remain"`
}

// Count grammar : either a positive integer literal, or an input
// reference of the form `input.<name>`. We accept neither raw HCL
// expressions nor empty input names — both broaden the surface past
// what Install can resolve.
var (
	countLiteralRE = regexp.MustCompile(`^[1-9][0-9]*$`)
	countInputRE   = regexp.MustCompile(`^input\.[a-z_][a-z0-9_]*$`)
)

// inputRefName extracts the `<name>` part of an `input.<name>` count
// value. Returns ("", false) for literal or empty counts.
func inputRefName(value string) (string, bool) {
	if !countInputRE.MatchString(value) {
		return "", false
	}
	return strings.TrimPrefix(value, "input."), true
}

// validateCount checks the textual form of a count attribute. An
// empty value is allowed (treated as "1" at resolve time).
func validateCount(field, value string) error {
	if value == "" {
		return nil
	}
	if countLiteralRE.MatchString(value) || countInputRE.MatchString(value) {
		return nil
	}
	return fmt.Errorf("%s = %q : expected a positive integer literal or an `input.<name>` reference", field, value)
}

// resolveCount turns the textual count into an integer, resolving
// `input.<name>` against the operator-supplied input map. Errors on :
// missing input, non-numeric value, count <= 0.
func resolveCount(value string, inputs map[string]string) (int, error) {
	if value == "" {
		return 1, nil
	}
	if countLiteralRE.MatchString(value) {
		n, err := strconv.Atoi(value)
		if err != nil {
			// regexp guarantees this won't happen ; stay defensive.
			return 0, fmt.Errorf("count %q : %w", value, err)
		}
		return n, nil
	}
	if countInputRE.MatchString(value) {
		key := strings.TrimPrefix(value, "input.")
		raw, ok := inputs[key]
		if !ok {
			return 0, fmt.Errorf("count = %s : input %q is not set", value, key)
		}
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return 0, fmt.Errorf("count = %s : input %q value %q is not an integer", value, key, raw)
		}
		if n <= 0 {
			return 0, fmt.Errorf("count = %s : input %q value %d must be > 0", value, key, n)
		}
		return n, nil
	}
	return 0, fmt.Errorf("count %q : unrecognised form (want literal or `input.<name>`)", value)
}

// buildCountEvalContext walks the parsed HCL body looking for
// `input "<name>" {}` blocks and returns an EvalContext whose `input`
// object maps each declared name to the literal string
// "input.<name>". That keeps `count = input.volumes_per_node` legal
// HCL syntax at decode time, where gohcl evaluates it and writes the
// reference back as a string on VMSpec.Count / VolumeSpec.Count. We
// resolve the actual integer at install time against the operator's
// supplied inputs (see resolveCount).
//
// We deliberately do NOT surface inputs anywhere else in the schema —
// only the count attribute reads from this context. Everything else
// stays a static decode against the textual default / supplied value.
func buildCountEvalContext(body hcl.Body) *hcl.EvalContext {
	names := scanInputNames(body)
	attrs := make(map[string]cty.Value, len(names))
	for _, n := range names {
		attrs[n] = cty.StringVal("input." + n)
	}
	if len(attrs) == 0 {
		return &hcl.EvalContext{
			Variables: map[string]cty.Value{"input": cty.EmptyObjectVal},
		}
	}
	return &hcl.EvalContext{
		Variables: map[string]cty.Value{"input": cty.ObjectVal(attrs)},
	}
}

// scanInputNames returns the set of `input "<name>" {}` block labels
// declared under any nested plugin block in body. Uses hclsyntax so
// we can introspect the AST without partially decoding it via gohcl
// (which would force us to commit to the wrapper struct twice).
func scanInputNames(body hcl.Body) []string {
	bb, ok := body.(*hclsyntax.Body)
	if !ok {
		return nil
	}
	var out []string
	for _, blk := range bb.Blocks {
		if blk.Type != "plugin" {
			continue
		}
		for _, inner := range blk.Body.Blocks {
			if inner.Type != "input" || len(inner.Labels) == 0 {
				continue
			}
			out = append(out, inner.Labels[0])
		}
	}
	return out
}

// ---------------------------------------------------------------
// Decoding
// ---------------------------------------------------------------

// LoadManifest reads a plugin.hcl file and returns the parsed
// manifest. The caller is responsible for resolving relative paths.
func LoadManifest(path string) (*Manifest, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", path, err)
	}
	return ParseManifest(filepath.Base(path), src)
}

// ParseManifest decodes manifest bytes. `filename` is used only for
// diagnostic messages — the contents drive everything else.
func ParseManifest(filename string, src []byte) (*Manifest, error) {
	// hclsimple wraps the same path but doesn't surface the
	// underlying hcl.File. We open-code so the caller can stitch
	// multiple manifests into one EvalContext later.
	file, diags := hclsyntax.ParseConfig(src, filename, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, diags
	}
	// Pre-scan input block labels so `count = input.<name>` decodes
	// to the literal string "input.<name>" via gohcl's eval pass.
	// We're not resolving the input value here — we just want the
	// operator-facing reference to survive into VMSpec.Count /
	// VolumeSpec.Count as text. resolveCount evaluates it at install
	// time against the operator's supplied inputs.
	ctx := buildCountEvalContext(file.Body)
	// The manifest is wrapped in a `plugin "<name>" { ... }`
	// block — same shape as Terraform's resource blocks. That
	// way the file is self-describing and the top-level Name is
	// the block label.
	var wrapper struct {
		Plugins []Manifest `hcl:"plugin,block"`
	}
	if diags := gohcl.DecodeBody(file.Body, ctx, &wrapper); diags.HasErrors() {
		return nil, diags
	}
	if len(wrapper.Plugins) != 1 {
		return nil, fmt.Errorf("plugin manifest %s: expected exactly one plugin block, got %d", filename, len(wrapper.Plugins))
	}
	m := wrapper.Plugins[0]
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plugin manifest %s: %w", filename, err)
	}
	// Normalise per-VM Replicas default — operators expect 3
	// for HA layouts but the manifest may leave the field unset.
	for i := range m.VMs {
		if m.VMs[i].Replicas == 0 {
			m.VMs[i].Replicas = defaultReplicasForLayout(m.Layout)
		}
	}
	return &m, nil
}

// Validate performs the static checks that don't depend on the
// runtime adapter (which Inputs are mandatory, which references
// resolve). Adapter-coupled checks happen inside Install.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("plugin manifest is missing the plugin block label (name)")
	}
	if m.Version == "" {
		return fmt.Errorf("plugin manifest %q: missing version", m.Name)
	}
	if m.Version != "v1" {
		return fmt.Errorf("plugin manifest %q: unsupported version %q (only v1)", m.Name, m.Version)
	}
	if m.Kind == "" {
		return fmt.Errorf("plugin manifest %q: missing kind", m.Name)
	}
	if len(m.VMs) == 0 {
		return fmt.Errorf("plugin manifest %q: needs at least one vm block", m.Name)
	}
	// Cross-reference network names — VM.network and SG.networks
	// must point at a real network block.
	known := map[string]struct{}{}
	for _, n := range m.Networks {
		if n.Name == "" {
			return fmt.Errorf("plugin manifest %q: network block is missing a label", m.Name)
		}
		if _, ok := known[n.Name]; ok {
			return fmt.Errorf("plugin manifest %q: duplicate network %q", m.Name, n.Name)
		}
		known[n.Name] = struct{}{}
	}
	for _, sg := range m.SecurityGroups {
		for _, ref := range sg.Networks {
			if _, ok := known[ref]; !ok {
				return fmt.Errorf("plugin manifest %q: security_group %q references unknown network %q", m.Name, sg.Name, ref)
			}
		}
		for _, r := range sg.Rules {
			if r.Direction != "ingress" && r.Direction != "egress" {
				return fmt.Errorf("plugin manifest %q: security_group %q rule has invalid direction %q", m.Name, sg.Name, r.Direction)
			}
		}
	}
	declaredInputs := map[string]struct{}{}
	for _, in := range m.Inputs {
		declaredInputs[in.Name] = struct{}{}
	}
	for _, v := range m.VMs {
		if v.Image == "" {
			return fmt.Errorf("plugin manifest %q: vm %q is missing an image reference", m.Name, v.Name)
		}
		if v.Network != "" {
			if _, ok := known[v.Network]; !ok {
				return fmt.Errorf("plugin manifest %q: vm %q references unknown network %q", m.Name, v.Name, v.Network)
			}
		}
		if err := validateCount(fmt.Sprintf("vm %q count", v.Name), v.Count); err != nil {
			return fmt.Errorf("plugin manifest %q: %w", m.Name, err)
		}
		if ref, ok := inputRefName(v.Count); ok {
			if _, declared := declaredInputs[ref]; !declared {
				return fmt.Errorf("plugin manifest %q: vm %q count references unknown input %q", m.Name, v.Name, ref)
			}
		}
		for _, vol := range v.Volumes {
			if err := validateCount(fmt.Sprintf("vm %q volume %q count", v.Name, vol.Name), vol.Count); err != nil {
				return fmt.Errorf("plugin manifest %q: %w", m.Name, err)
			}
			if ref, ok := inputRefName(vol.Count); ok {
				if _, declared := declaredInputs[ref]; !declared {
					return fmt.Errorf("plugin manifest %q: vm %q volume %q count references unknown input %q", m.Name, v.Name, vol.Name, ref)
				}
			}
		}
	}
	// Sort Inputs so error messages and `plugin list` output
	// stay stable across map iterations elsewhere.
	sort.SliceStable(m.Inputs, func(i, j int) bool { return m.Inputs[i].Name < m.Inputs[j].Name })
	return nil
}

// ValidateInputs resolves the operator-supplied k=v map against the
// manifest's input declarations. Returns the resolved map (with
// defaults filled in) or an error naming the first missing required
// input. The returned map is keyed by Input.Name and the values are
// strings — typed coercion is a callsite concern.
func (m *Manifest) ValidateInputs(supplied map[string]any) (map[string]string, error) {
	declared := map[string]Input{}
	for _, in := range m.Inputs {
		declared[in.Name] = in
	}
	for k := range supplied {
		if _, ok := declared[k]; !ok {
			return nil, fmt.Errorf("plugin %q: unknown input %q", m.Name, k)
		}
	}
	out := make(map[string]string, len(declared))
	for name, in := range declared {
		if v, ok := supplied[name]; ok {
			out[name] = fmt.Sprintf("%v", v)
			continue
		}
		if in.Default != "" {
			out[name] = in.Default
			continue
		}
		if in.Required {
			return nil, fmt.Errorf("plugin %q: missing required input %q", m.Name, name)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------
// Catalogue lookup
// ---------------------------------------------------------------

// LoadCatalogue scans a directory of plugin folders (each containing
// plugin.hcl) and returns the parsed manifests keyed by name. Folder
// names that don't carry a plugin.hcl file are skipped silently —
// they're assumed to be docs or scratch space.
func LoadCatalogue(root string) (map[string]*Manifest, error) {
	out := map[string]*Manifest{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read catalogue root %s: %w", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "plugin.hcl")
		if _, statErr := os.Stat(path); statErr != nil {
			continue
		}
		m, err := LoadManifest(path)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
		if _, dup := out[m.Name]; dup {
			return nil, fmt.Errorf("duplicate plugin %q at %s", m.Name, path)
		}
		out[m.Name] = m
	}
	return out, nil
}

// DefaultCatalogueRoot resolves the catalogue/ directory next to the
// running weft binary's source tree. The CLI accepts an override via
// $WEFT_CATALOGUE_DIR ; tests point this at a t.TempDir.
func DefaultCatalogueRoot() string {
	if v := os.Getenv("WEFT_CATALOGUE_DIR"); v != "" {
		return v
	}
	// In dev (`go test`) the cwd is the package directory; the
	// catalogue lives two levels up.
	wd, err := os.Getwd()
	if err != nil {
		return "catalogue"
	}
	// Climb until we find a "catalogue" sibling directory, or
	// fall back to "catalogue" relative to cwd.
	for cur := wd; cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		candidate := filepath.Join(cur, "catalogue")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return "catalogue"
}

func defaultReplicasForLayout(layout string) int {
	if strings.EqualFold(layout, "ha-3dc") || layout == "" {
		return 3
	}
	return 1
}
