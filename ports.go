package weft

// ports.go owns the per-project port registry. A Port = one VM
// NIC attached to one Network, with a MAC + IP within the
// network's CIDR. For mesh-type networks the Port also carries
// the per-VM WireGuard public key (the private key never leaves
// the VM).
//
// Ports decouple the (VM, Network) relationship from the VM and
// Network records themselves: a VM can have N ports (one per
// network it's attached to), and per-port security groups
// override the network-default SG list.
//
// Schema:
//
//   port "abc-…" {
//     project_uuid       = "p-…"
//     vm_uuid            = "vm-…"
//     network_uuid       = "net-…"
//     mac                = "52:54:00:..."
//     ip                 = "10.42.0.5"
//     wireguard_pub_key  = "..."           # mesh only
//     mesh_endpoint      = "host:51820"    # mesh only, per-port override
//     security_groups    = ["sg-…", …]     # optional override; empty = inherit Network.DefaultSecurityGroups
//     created_at         = "..."
//   }
//
// Cross-registry validation (all in the Adapter wrapper, not the
// registry):
//
//   * VM exists in the same project (Phase F VM-inventory will
//     enforce; for now we just record the UUID without lookup —
//     VM "registry" today is the on-disk vmDir).
//   * Network exists in the same project.
//   * IP is inside Network.CIDR and not already taken by another
//     port on that network.
//   * MAC is unique on the network.
//   * Each security_group exists in the same project.
//   * mesh-only fields (wireguard_pub_key, mesh_endpoint) are
//     refused when Network.Type != mesh.
//
// See [[wireguard-mesh-networks]] for the broader design.

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// Port is one entry in the port registry.
type Port struct {
	UUID            string    `json:"uuid"`
	ProjectUUID     string    `json:"project_uuid"`
	VMUUID          string    `json:"vm_uuid"`
	NetworkUUID     string    `json:"network_uuid"`
	MAC             string    `json:"mac"`
	IP              string    `json:"ip"`
	WireguardPubKey string    `json:"wireguard_pub_key,omitempty"`
	MeshEndpoint    string    `json:"mesh_endpoint,omitempty"`
	SecurityGroups  []string  `json:"security_groups,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// portsDoc is the top-level HCL schema.
type portsDoc struct {
	Ports []portBlock `hcl:"port,block"`
}

type portBlock struct {
	UUID            string   `hcl:",label"`
	ProjectUUID     string   `hcl:"project_uuid"`
	VMUUID          string   `hcl:"vm_uuid"`
	NetworkUUID     string   `hcl:"network_uuid"`
	MAC             string   `hcl:"mac"`
	IP              string   `hcl:"ip"`
	WireguardPubKey string   `hcl:"wireguard_pub_key,optional"`
	MeshEndpoint    string   `hcl:"mesh_endpoint,optional"`
	SecurityGroups  []string `hcl:"security_groups,optional"`
	CreatedAt       string   `hcl:"created_at"`
}

// portRegistry indexes by multiple keys because every lookup
// path is hot: by UUID (admin), by VM (boot config render),
// by network (mesh peer-set propagation), by (network,IP) and
// (network,MAC) for uniqueness checks.
type portRegistry struct {
	mu         sync.Mutex
	storage    Storage
	byUUID     map[string]Port
	vmIdx      map[string]map[string]struct{} // vmUUID → set-of-portUUIDs
	networkIdx map[string]map[string]struct{} // networkUUID → set-of-portUUIDs
	ipIdx      map[string]string              // (networkUUID,ip) → portUUID
	macIdx     map[string]string              // (networkUUID,mac) → portUUID
}

func loadPortRegistry(ctx context.Context, storage Storage) (*portRegistry, error) {
	reg := &portRegistry{
		storage:    storage,
		byUUID:     make(map[string]Port),
		vmIdx:      make(map[string]map[string]struct{}),
		networkIdx: make(map[string]map[string]struct{}),
		ipIdx:      make(map[string]string),
		macIdx:     make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load port registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc portsDoc
	if err := hclsimple.Decode("port-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse port registry: %w", err)
	}
	for _, b := range doc.Ports {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		p := Port{
			UUID:            b.UUID,
			ProjectUUID:     b.ProjectUUID,
			VMUUID:          b.VMUUID,
			NetworkUUID:     b.NetworkUUID,
			MAC:             b.MAC,
			IP:              b.IP,
			WireguardPubKey: b.WireguardPubKey,
			MeshEndpoint:    b.MeshEndpoint,
			SecurityGroups:  append([]string(nil), b.SecurityGroups...),
			CreatedAt:       created,
		}
		reg.indexLocked(p)
	}
	return reg, nil
}

// indexLocked adds p to every secondary index. Caller holds mu
// (or is in the load path before publication).
func (r *portRegistry) indexLocked(p Port) {
	r.byUUID[p.UUID] = p
	if _, ok := r.vmIdx[p.VMUUID]; !ok {
		r.vmIdx[p.VMUUID] = make(map[string]struct{})
	}
	r.vmIdx[p.VMUUID][p.UUID] = struct{}{}
	if _, ok := r.networkIdx[p.NetworkUUID]; !ok {
		r.networkIdx[p.NetworkUUID] = make(map[string]struct{})
	}
	r.networkIdx[p.NetworkUUID][p.UUID] = struct{}{}
	r.ipIdx[portIPKey(p.NetworkUUID, p.IP)] = p.UUID
	r.macIdx[portMACKey(p.NetworkUUID, p.MAC)] = p.UUID
}

// unindexLocked removes p from every secondary index. Caller
// holds mu.
func (r *portRegistry) unindexLocked(p Port) {
	delete(r.byUUID, p.UUID)
	delete(r.vmIdx[p.VMUUID], p.UUID)
	if len(r.vmIdx[p.VMUUID]) == 0 {
		delete(r.vmIdx, p.VMUUID)
	}
	delete(r.networkIdx[p.NetworkUUID], p.UUID)
	if len(r.networkIdx[p.NetworkUUID]) == 0 {
		delete(r.networkIdx, p.NetworkUUID)
	}
	delete(r.ipIdx, portIPKey(p.NetworkUUID, p.IP))
	delete(r.macIdx, portMACKey(p.NetworkUUID, p.MAC))
}

func portIPKey(networkUUID, ip string) string  { return networkUUID + "\x00" + ip }
func portMACKey(networkUUID, mac string) string { return networkUUID + "\x00" + mac }

// saveLocked writes the registry via Storage. Caller holds mu.
func (r *portRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft port registry — UUID-keyed per [[weft-uuid-keyed-resources]].\n" +
				"# Each `port` block links one VM NIC to one network with its MAC/IP\n" +
				"# (and WireGuard pubkey for mesh-type networks).\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		p := r.byUUID[u]
		block := body.AppendNewBlock("port", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("project_uuid", cty.StringVal(p.ProjectUUID))
		bb.SetAttributeValue("vm_uuid", cty.StringVal(p.VMUUID))
		bb.SetAttributeValue("network_uuid", cty.StringVal(p.NetworkUUID))
		bb.SetAttributeValue("mac", cty.StringVal(p.MAC))
		bb.SetAttributeValue("ip", cty.StringVal(p.IP))
		if p.WireguardPubKey != "" {
			bb.SetAttributeValue("wireguard_pub_key", cty.StringVal(p.WireguardPubKey))
		}
		if p.MeshEndpoint != "" {
			bb.SetAttributeValue("mesh_endpoint", cty.StringVal(p.MeshEndpoint))
		}
		if len(p.SecurityGroups) > 0 {
			vals := make([]cty.Value, len(p.SecurityGroups))
			for i, s := range p.SecurityGroups {
				vals[i] = cty.StringVal(s)
			}
			bb.SetAttributeValue("security_groups", cty.ListVal(vals))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(p.CreatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// lookupByUUID returns (Port, true) when the UUID is known.
func (r *portRegistry) lookupByUUID(uuid string) (Port, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	return p, ok
}

// listForVM returns every port attached to the given VM, sorted
// by NetworkUUID. Used to render the per-VM `wg.conf` + the
// virtio-net device list at boot.
func (r *portRegistry) listForVM(vmUUID string) []Port {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.vmIdx[vmUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]Port, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NetworkUUID < out[j].NetworkUUID })
	return out
}

// listForNetwork returns every port on the given network, sorted
// by IP. This is the peer set used for mesh-config rendering
// (each peer-block in wg.conf comes from one Port on the same
// mesh network).
func (r *portRegistry) listForNetwork(networkUUID string) []Port {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.networkIdx[networkUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]Port, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// list returns every port, sorted by (ProjectUUID, NetworkUUID, IP).
func (r *portRegistry) list() []Port {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Port, 0, len(r.byUUID))
	for _, p := range r.byUUID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		if out[i].NetworkUUID != out[j].NetworkUUID {
			return out[i].NetworkUUID < out[j].NetworkUUID
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// CreatePortSpec carries the inputs for create. Pre-validated
// at the Adapter level (network exists, IP in CIDR, no collisions
// — see Adapter.CreatePort).
type CreatePortSpec struct {
	ProjectUUID     string
	VMUUID          string
	NetworkUUID     string
	MAC             string
	IP              string
	WireguardPubKey string // mesh only; empty otherwise
	MeshEndpoint    string // mesh only; empty otherwise
	SecurityGroups  []string
}

// create registers a new Port. The registry enforces only
// internal uniqueness invariants (IP, MAC per network); broader
// cross-registry validation (network exists, SG project match,
// mesh-only field gating) is the Adapter's responsibility.
func (r *portRegistry) create(spec CreatePortSpec) (Port, error) {
	if spec.ProjectUUID == "" {
		return Port{}, fmt.Errorf("empty project_uuid")
	}
	if spec.VMUUID == "" {
		return Port{}, fmt.Errorf("empty vm_uuid")
	}
	if spec.NetworkUUID == "" {
		return Port{}, fmt.Errorf("empty network_uuid")
	}
	if spec.MAC == "" {
		return Port{}, fmt.Errorf("empty mac")
	}
	if spec.IP == "" {
		return Port{}, fmt.Errorf("empty ip")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, taken := r.ipIdx[portIPKey(spec.NetworkUUID, spec.IP)]; taken {
		return Port{}, fmt.Errorf("ip %s already taken on network %s by port %s", spec.IP, spec.NetworkUUID, existing)
	}
	if existing, taken := r.macIdx[portMACKey(spec.NetworkUUID, spec.MAC)]; taken {
		return Port{}, fmt.Errorf("mac %s already taken on network %s by port %s", spec.MAC, spec.NetworkUUID, existing)
	}
	p := Port{
		UUID:            newUUID(),
		ProjectUUID:     spec.ProjectUUID,
		VMUUID:          spec.VMUUID,
		NetworkUUID:     spec.NetworkUUID,
		MAC:             spec.MAC,
		IP:              spec.IP,
		WireguardPubKey: spec.WireguardPubKey,
		MeshEndpoint:    spec.MeshEndpoint,
		SecurityGroups:  append([]string(nil), spec.SecurityGroups...),
		CreatedAt:       time.Now().UTC(),
	}
	r.indexLocked(p)
	if err := r.saveLocked(); err != nil {
		r.unindexLocked(p)
		return Port{}, err
	}
	return p, nil
}

// setSecurityGroups replaces the per-port SG override list. Pure
// storage op; cross-registry validation is at the Adapter level.
func (r *portRegistry) setSecurityGroups(uuid string, sgUUIDs []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("port %q not found", uuid)
	}
	p.SecurityGroups = append([]string(nil), sgUUIDs...)
	r.byUUID[uuid] = p
	return r.saveLocked()
}

// setWireguardPubKey rotates the per-port WireGuard public key.
// Used when an operator rotates a VM's keypair without recreating
// the port. The private key stays inside the VM — weft only sees
// the new pubkey via the in-VM agent.
func (r *portRegistry) setWireguardPubKey(uuid, pubkey string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("port %q not found", uuid)
	}
	p.WireguardPubKey = pubkey
	r.byUUID[uuid] = p
	return r.saveLocked()
}

// delete drops a port from the registry. Indexes are cleared
// from all four secondary maps.
func (r *portRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("port %q not found", uuid)
	}
	r.unindexLocked(p)
	return r.saveLocked()
}

// portsReferencingSecurityGroup returns the UUIDs of every port
// that lists the given SG in its per-port override list. Used by
// the Adapter to refuse SG deletion when still referenced — same
// safety mechanism as networksReferencingSecurityGroup.
func (r *portRegistry) portsReferencingSecurityGroup(sgUUID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var refs []string
	for _, p := range r.byUUID {
		for _, ref := range p.SecurityGroups {
			if ref == sgUUID {
				refs = append(refs, p.UUID)
				break
			}
		}
	}
	return refs
}
