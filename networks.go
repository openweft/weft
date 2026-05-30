package weft

// networks.go owns the per-project network registry: virtual
// L3 networks the platform provisions, attached to VMs at start
// time.
//
// Differences from projects.go / users.go:
//
//   * Networks are SCOPED to a project — two projects can both
//     own a network called "default" without colliding. The
//     secondary index is therefore (projectUUID, name) → UUID,
//     not just name → UUID.
//   * The struct carries L3 config (CIDR, gateway, DNS servers,
//     type) so the actual VZ network attachment can be built at
//     VM-start time without touching the registry again.
//
// Wire model:
//
//   * registry blob: <vmsDir>/.networks.hcl (or etcd key
//     <prefix>/networks)
//   * one `network "<uuid>" { ... }` block per entry
//   * Storage interface decides where the blob lives
//
// HCL schema:
//
//   network "abc-…" {
//     project_uuid = "p-…"
//     name         = "default"
//     cidr         = "10.42.0.0/24"
//     gateway      = "10.42.0.1"
//     dns_servers  = ["1.1.1.1", "9.9.9.9"]
//     type         = "nat"                # nat | bridged | isolated
//     created_at   = "..."
//   }
//
// Phase D of [[etcd-control-plane]]: every new registry is
// Storage-backed from day one.

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// NetworkType enumerates the network attachment shapes weft
// understands. Validation lives in (registry).validateType so the
// "list of allowed values" is in one place.
type NetworkType string

const (
	NetworkTypeNAT      NetworkType = "nat"      // host-shared NAT, default for new projects
	NetworkTypeBridged  NetworkType = "bridged"  // bridges onto a host interface
	NetworkTypeIsolated NetworkType = "isolated" // VM-to-VM only, no host / external reach
	// NetworkTypeMesh = WireGuard-backed L3 overlay between VMs,
	// potentially across hosts / AZs. Per-network key + endpoint
	// are stored on Network; per-VM keypair lives on the Port
	// registry (Phase E). See [[wireguard-mesh-networks]] for
	// the design decision and constraints.
	NetworkTypeMesh NetworkType = "mesh"
)

// Network is one entry in the network registry. UUID,
// ProjectUUID, CIDR and Type are immutable; Name, DNS list, and
// DefaultSecurityGroups can be updated via the dedicated setters.
//
// DefaultSecurityGroups is the list of security-group UUIDs
// auto-attached to every VM port created on this network. Per-VM
// overrides will come later (Phase E port-registry). Validation
// that referenced SGs exist + belong to the same project happens
// in the Adapter wrapper, not the registry — the registry stays
// dependency-free.
//
// Mesh-specific fields (MeshListenPort, MeshEndpoint) are only
// meaningful when Type == NetworkTypeMesh. The per-VM WireGuard
// keypair lives on the future Port registry, not here — Network
// only carries network-wide settings. See [[wireguard-mesh-networks]].
type Network struct {
	UUID                  string      `json:"uuid"`
	ProjectUUID           string      `json:"project_uuid"`
	Name                  string      `json:"name"`
	CIDR                  string      `json:"cidr"`
	Gateway               string      `json:"gateway"`
	DNSServers            []string    `json:"dns_servers"`
	Type                  NetworkType `json:"type"`
	DefaultSecurityGroups []string    `json:"default_security_groups,omitempty"`
	// MeshListenPort is the UDP port WireGuard listens on for
	// incoming peer connections. 0 means "let the kernel pick"
	// (only meaningful for VMs that never receive incoming
	// connections). Conventional default: 51820.
	MeshListenPort int `json:"mesh_listen_port,omitempty"`
	// MeshEndpoint is the public host:port a peer should dial to
	// reach VMs on this network from outside their host / AZ.
	// Empty for purely intra-host meshes. Each VM's Port can also
	// carry its own per-port endpoint override (Phase E).
	MeshEndpoint string    `json:"mesh_endpoint,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// networksDoc is the top-level HCL schema decoded from the
// registry blob. One `network "<uuid>" { ... }` block per entry.
type networksDoc struct {
	Networks []networkBlock `hcl:"network,block"`
}

// networkBlock mirrors one HCL block. Label = UUID.
type networkBlock struct {
	UUID                  string   `hcl:",label"`
	ProjectUUID           string   `hcl:"project_uuid"`
	Name                  string   `hcl:"name"`
	CIDR                  string   `hcl:"cidr"`
	Gateway               string   `hcl:"gateway,optional"`
	DNSServers            []string `hcl:"dns_servers,optional"`
	Type                  string   `hcl:"type,optional"`
	DefaultSecurityGroups []string `hcl:"default_security_groups,optional"`
	MeshListenPort        int      `hcl:"mesh_listen_port,optional"`
	MeshEndpoint          string   `hcl:"mesh_endpoint,optional"`
	CreatedAt             string   `hcl:"created_at"`
}

// networkRegistry mirrors projectRegistry / userRegistry.
//
// Indexes:
//
//   byUUID:     primary, every public lookup goes through this.
//   nameIdx:    (projectUUID,name) → UUID — name scoped per
//               project. The composite key uses NUL as
//               separator (UUID chars + names never contain NUL).
//   projectIdx: projectUUID → set-of-UUIDs — used by
//               ListNetworksForProject; the value is a map for
//               O(1) delete on rename / drop.
type networkRegistry struct {
	mu         sync.Mutex
	storage    Storage
	byUUID     map[string]Network
	nameIdx    map[string]string
	projectIdx map[string]map[string]struct{}
}

// loadNetworkRegistry reads the blob via Storage. Empty / absent
// Storage yields an empty registry — Adapter populates it on
// demand.
func loadNetworkRegistry(ctx context.Context, storage Storage) (*networkRegistry, error) {
	reg := &networkRegistry{
		storage:    storage,
		byUUID:     make(map[string]Network),
		nameIdx:    make(map[string]string),
		projectIdx: make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load network registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc networksDoc
	if err := hclsimple.Decode("network-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse network registry: %w", err)
	}
	for _, b := range doc.Networks {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		typ := NetworkType(b.Type)
		if typ == "" {
			typ = NetworkTypeNAT
		}
		n := Network{
			UUID:                  b.UUID,
			ProjectUUID:           b.ProjectUUID,
			Name:                  b.Name,
			CIDR:                  b.CIDR,
			Gateway:               b.Gateway,
			DNSServers:            append([]string(nil), b.DNSServers...),
			Type:                  typ,
			DefaultSecurityGroups: append([]string(nil), b.DefaultSecurityGroups...),
			MeshListenPort:        b.MeshListenPort,
			MeshEndpoint:          b.MeshEndpoint,
			CreatedAt:             created,
		}
		reg.byUUID[n.UUID] = n
		reg.nameIdx[networkNameKey(n.ProjectUUID, n.Name)] = n.UUID
		if _, ok := reg.projectIdx[n.ProjectUUID]; !ok {
			reg.projectIdx[n.ProjectUUID] = make(map[string]struct{})
		}
		reg.projectIdx[n.ProjectUUID][n.UUID] = struct{}{}
	}
	return reg, nil
}

// networkNameKey composes the (projectUUID, name) secondary
// index key. NUL is a safe separator — neither component
// contains it.
func networkNameKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
}

// saveLocked writes the registry via Storage. Caller must hold
// mu. HCL is sorted by UUID for stable diffs.
func (r *networkRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft network registry — UUID-keyed per [[weft-uuid-keyed-resources]].\n" +
				"# Edit `name` or `dns_servers` freely; never change the block label (UUID),\n" +
				"# `project_uuid`, `cidr`, or `type`.\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		n := r.byUUID[u]
		block := body.AppendNewBlock("network", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("project_uuid", cty.StringVal(n.ProjectUUID))
		bb.SetAttributeValue("name", cty.StringVal(n.Name))
		bb.SetAttributeValue("cidr", cty.StringVal(n.CIDR))
		if n.Gateway != "" {
			bb.SetAttributeValue("gateway", cty.StringVal(n.Gateway))
		}
		if len(n.DNSServers) > 0 {
			vals := make([]cty.Value, len(n.DNSServers))
			for i, s := range n.DNSServers {
				vals[i] = cty.StringVal(s)
			}
			bb.SetAttributeValue("dns_servers", cty.ListVal(vals))
		}
		if n.Type != "" {
			bb.SetAttributeValue("type", cty.StringVal(string(n.Type)))
		}
		if len(n.DefaultSecurityGroups) > 0 {
			vals := make([]cty.Value, len(n.DefaultSecurityGroups))
			for i, s := range n.DefaultSecurityGroups {
				vals[i] = cty.StringVal(s)
			}
			bb.SetAttributeValue("default_security_groups", cty.ListVal(vals))
		}
		if n.MeshListenPort != 0 {
			bb.SetAttributeValue("mesh_listen_port", cty.NumberIntVal(int64(n.MeshListenPort)))
		}
		if n.MeshEndpoint != "" {
			bb.SetAttributeValue("mesh_endpoint", cty.StringVal(n.MeshEndpoint))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(n.CreatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// validateType refuses unknown NetworkType values. Default empty
// type is allowed (treated as NAT at create time).
func validateNetworkType(t NetworkType) error {
	switch t {
	case "", NetworkTypeNAT, NetworkTypeBridged, NetworkTypeIsolated, NetworkTypeMesh:
		return nil
	default:
		return fmt.Errorf("unknown network type %q (want nat, bridged, isolated, or mesh)", t)
	}
}

// validateMeshFields enforces the constraints around mesh-only
// fields:
//   - port in [0, 65535] (0 = "let kernel pick")
//   - endpoint must be host:port format when set
//   - port + endpoint may only be non-zero when Type == mesh
func validateMeshFields(typ NetworkType, port int, endpoint string) error {
	if typ != NetworkTypeMesh {
		if port != 0 {
			return fmt.Errorf("mesh_listen_port is only valid when type == mesh")
		}
		if endpoint != "" {
			return fmt.Errorf("mesh_endpoint is only valid when type == mesh")
		}
		return nil
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("mesh_listen_port out of range [0,65535]: %d", port)
	}
	if endpoint != "" {
		// host:port — net.SplitHostPort handles IPv6 brackets too.
		host, portStr, err := net.SplitHostPort(endpoint)
		if err != nil {
			return fmt.Errorf("mesh_endpoint %q: %w", endpoint, err)
		}
		if host == "" {
			return fmt.Errorf("mesh_endpoint %q: empty host", endpoint)
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 || p > 65535 {
			return fmt.Errorf("mesh_endpoint %q: invalid port", endpoint)
		}
	}
	return nil
}

// validateCIDR parses the CIDR + optional Gateway. Empty Gateway
// is allowed (caller defaults to .1 in the CIDR when omitted).
// Returns the normalised CIDR (canonical form).
func validateCIDR(cidr, gateway string) (string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", fmt.Errorf("cidr %q: %w", cidr, err)
	}
	if gateway != "" {
		gw := net.ParseIP(gateway)
		if gw == nil {
			return "", fmt.Errorf("gateway %q: invalid IP", gateway)
		}
		if !ipnet.Contains(gw) {
			return "", fmt.Errorf("gateway %q is outside cidr %s", gateway, ipnet.String())
		}
	}
	return ipnet.String(), nil
}

// lookupByUUID returns (Network, true) when the UUID is known.
func (r *networkRegistry) lookupByUUID(uuid string) (Network, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byUUID[uuid]
	return n, ok
}

// lookupByName resolves (projectUUID, name) → Network. Cross-
// project name collisions are valid by design — each project has
// its own namespace.
func (r *networkRegistry) lookupByName(projectUUID, name string) (Network, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[networkNameKey(projectUUID, name)]
	if !ok {
		return Network{}, false
	}
	n, ok := r.byUUID[uuid]
	return n, ok
}

// listForProject returns every network owned by the given
// project, sorted by name. Empty slice when the project has none.
func (r *networkRegistry) listForProject(projectUUID string) []Network {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.projectIdx[projectUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]Network, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// list returns every registered network across all projects,
// sorted by (ProjectUUID, Name) for stable test + tabular output.
func (r *networkRegistry) list() []Network {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Network, 0, len(r.byUUID))
	for _, n := range r.byUUID {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CreateNetworkSpec carries the inputs to create. Kept as a
// struct so the constructor signature doesn't drift every time a
// new optional field is added (DNS servers, MTU, …).
//
// MeshListenPort + MeshEndpoint are only consulted when
// Type == NetworkTypeMesh; setting them on any other type is
// rejected by validateMeshFields.
type CreateNetworkSpec struct {
	ProjectUUID    string
	Name           string
	CIDR           string
	Gateway        string
	DNSServers     []string
	Type           NetworkType
	MeshListenPort int
	MeshEndpoint   string
}

// create registers a new Network under the given project.
// Refuses name collisions within the same project. Refuses
// invalid CIDR / gateway combinations.
func (r *networkRegistry) create(spec CreateNetworkSpec) (Network, error) {
	if spec.ProjectUUID == "" {
		return Network{}, fmt.Errorf("empty project_uuid")
	}
	if spec.Name == "" {
		return Network{}, fmt.Errorf("empty network name")
	}
	if err := validateNetworkType(spec.Type); err != nil {
		return Network{}, err
	}
	// Resolve effective type for the mesh-field check: empty
	// type defaults to NAT, so a non-mesh mesh-field combo is
	// caught here even when Type was left blank.
	effectiveType := spec.Type
	if effectiveType == "" {
		effectiveType = NetworkTypeNAT
	}
	if err := validateMeshFields(effectiveType, spec.MeshListenPort, spec.MeshEndpoint); err != nil {
		return Network{}, err
	}
	canonicalCIDR, err := validateCIDR(spec.CIDR, spec.Gateway)
	if err != nil {
		return Network{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := networkNameKey(spec.ProjectUUID, spec.Name)
	if _, taken := r.nameIdx[key]; taken {
		return Network{}, fmt.Errorf("network name %q already in use in project %s", spec.Name, spec.ProjectUUID)
	}
	typ := spec.Type
	if typ == "" {
		typ = NetworkTypeNAT
	}
	n := Network{
		UUID:           newUUID(),
		ProjectUUID:    spec.ProjectUUID,
		Name:           spec.Name,
		CIDR:           canonicalCIDR,
		Gateway:        spec.Gateway,
		DNSServers:     append([]string(nil), spec.DNSServers...),
		Type:           typ,
		MeshListenPort: spec.MeshListenPort,
		MeshEndpoint:   spec.MeshEndpoint,
		CreatedAt:      time.Now().UTC(),
	}
	r.byUUID[n.UUID] = n
	r.nameIdx[key] = n.UUID
	if _, ok := r.projectIdx[n.ProjectUUID]; !ok {
		r.projectIdx[n.ProjectUUID] = make(map[string]struct{})
	}
	r.projectIdx[n.ProjectUUID][n.UUID] = struct{}{}
	if err := r.saveLocked(); err != nil {
		// Roll back to keep memory ↔ Storage in sync.
		delete(r.byUUID, n.UUID)
		delete(r.nameIdx, key)
		delete(r.projectIdx[n.ProjectUUID], n.UUID)
		if len(r.projectIdx[n.ProjectUUID]) == 0 {
			delete(r.projectIdx, n.ProjectUUID)
		}
		return Network{}, err
	}
	return n, nil
}

// setName renames a network within its project. Refuses name
// collisions. (ProjectUUID, UUID, CIDR, Type) are immutable.
func (r *networkRegistry) setName(uuid, newName string) error {
	if newName == "" {
		return fmt.Errorf("empty network name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("network %q not found", uuid)
	}
	if n.Name == newName {
		return nil
	}
	newKey := networkNameKey(n.ProjectUUID, newName)
	if existing, taken := r.nameIdx[newKey]; taken && existing != uuid {
		return fmt.Errorf("network name %q already in use in project %s", newName, n.ProjectUUID)
	}
	delete(r.nameIdx, networkNameKey(n.ProjectUUID, n.Name))
	n.Name = newName
	r.byUUID[uuid] = n
	r.nameIdx[newKey] = uuid
	return r.saveLocked()
}

// setDNSServers updates the DNS list. Pass a nil / empty slice to
// clear.
func (r *networkRegistry) setDNSServers(uuid string, servers []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("network %q not found", uuid)
	}
	n.DNSServers = append([]string(nil), servers...)
	r.byUUID[uuid] = n
	return r.saveLocked()
}

// setDefaultSecurityGroups replaces the default-SG list. Pure
// storage operation: the Adapter wrapper validates that each
// SG UUID exists in the same project before calling.
func (r *networkRegistry) setDefaultSecurityGroups(uuid string, sgUUIDs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("network %q not found", uuid)
	}
	n.DefaultSecurityGroups = append([]string(nil), sgUUIDs...)
	r.byUUID[uuid] = n
	return r.saveLocked()
}

// networksReferencingSecurityGroup returns the UUIDs of every
// network that lists the given SG as a default. Used by the
// Adapter to refuse SG deletion when still referenced.
func (r *networkRegistry) networksReferencingSecurityGroup(sgUUID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var refs []string
	for _, n := range r.byUUID {
		for _, ref := range n.DefaultSecurityGroups {
			if ref == sgUUID {
				refs = append(refs, n.UUID)
				break
			}
		}
	}
	return refs
}

// delete drops a network from the registry. Callers should ensure
// no running VM still attaches to it — there's no cascade here.
func (r *networkRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("network %q not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, networkNameKey(n.ProjectUUID, n.Name))
	delete(r.projectIdx[n.ProjectUUID], uuid)
	if len(r.projectIdx[n.ProjectUUID]) == 0 {
		delete(r.projectIdx, n.ProjectUUID)
	}
	return r.saveLocked()
}
