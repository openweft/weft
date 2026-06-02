package weft

// floating_ips.go owns the platform's pool of public-routable
// (or otherwise edge-side) addresses that can be bound to a VM's
// private port to expose it. Same OpenStack-equivalent model as
// the proto's FloatingIPInfo : an address is "available" until a
// MapFloatingIP attaches it to a target, then "active" until an
// UnmapFloatingIP releases the binding.
//
// Schema :
//
//   floating_ip "abc-…" {
//     project_uuid = "p-…"
//     network_uuid = "edge-net-…"   # which network the address belongs to
//     address      = "203.0.113.42" # the actual IPv4/IPv6
//     mapped_to    = "vm-web-1"     # "" when status == "available"
//     target_kind  = "vm"           # "vm" | "lb" ; "" when unmapped
//     status       = "available"    # "available" | "active"
//     allocated_at = "..."
//   }
//
// Lifecycle : Allocate → (Map → Unmap)* → Release. Map / Unmap are
// idempotent on the value of mapped_to (a no-op when already at
// the requested state). Allocate picks the next free address in
// the chosen network's CIDR that isn't already taken by another
// FloatingIP on the same network — Port-occupied addresses are
// also excluded so we never hand out a private-IP collision.
//
// (UUID, ProjectUUID, NetworkUUID, Address, AllocatedAt) immutable
// once allocated ; mapped_to / target_kind / status mutable via the
// Map / Unmap helpers.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// FloatingIPStatus enumerates the lifecycle states the public API
// surfaces. Kept narrow on purpose — "active" vs "available" is
// the only distinction the tenant UI cares about.
type FloatingIPStatus string

const (
	// FIPStatusAvailable : allocated but not yet bound to a target.
	FIPStatusAvailable FloatingIPStatus = "available"
	// FIPStatusActive : bound to a VM or LB target ; the
	// data-plane reconciler is expected to be installing the
	// NAT translation.
	FIPStatusActive FloatingIPStatus = "active"
)

// FloatingIPTargetKind enumerates what kind of resource a FIP can
// point at. "vm" is the v0 target ; "lb" is reserved for the
// load-balancer integration that will land alongside the data-
// plane reconciler.
type FloatingIPTargetKind string

const (
	FIPTargetVM FloatingIPTargetKind = "vm"
	FIPTargetLB FloatingIPTargetKind = "lb"
)

// FloatingIP is one entry in the registry.
type FloatingIP struct {
	UUID        string               `json:"uuid"`
	ProjectUUID string               `json:"project_uuid"`
	NetworkUUID string               `json:"network_uuid"`
	Address     string               `json:"address"`
	MappedTo    string               `json:"mapped_to,omitempty"`
	TargetKind  FloatingIPTargetKind `json:"target_kind,omitempty"`
	Status      FloatingIPStatus     `json:"status"`
	AllocatedAt time.Time            `json:"allocated_at"`
}

// HCL document structure.
type floatingIPsDoc struct {
	FloatingIPs []floatingIPBlock `hcl:"floating_ip,block"`
}

type floatingIPBlock struct {
	UUID        string `hcl:",label"`
	ProjectUUID string `hcl:"project_uuid"`
	NetworkUUID string `hcl:"network_uuid"`
	Address     string `hcl:"address"`
	MappedTo    string `hcl:"mapped_to,optional"`
	TargetKind  string `hcl:"target_kind,optional"`
	Status      string `hcl:"status"`
	AllocatedAt string `hcl:"allocated_at"`
}

// floatingIPRegistry indexes by UUID (admin lookup), by address
// (collision check on allocation), by (project, network) (Allocate
// + List), and by mapped target (the reconciler asks "what FIPs
// point at this VM ?"). Locking is per-registry, not per-FIP — all
// the methods are short.
type floatingIPRegistry struct {
	mu          sync.Mutex
	storage     Storage
	byUUID      map[string]FloatingIP
	addrIdx     map[string]string                       // (networkUUID,address) → UUID
	projectIdx  map[string]map[string]struct{}          // projectUUID → set-of-UUIDs
	targetIdx   map[string]map[string]struct{}          // (kind,target) → set-of-UUIDs
}

func loadFloatingIPRegistry(ctx context.Context, storage Storage) (*floatingIPRegistry, error) {
	reg := newFloatingIPRegistry(storage)
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load floating-ip registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc floatingIPsDoc
	if err := hclsimple.Decode("floating-ips.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse floating-ip registry: %w", err)
	}
	for _, b := range doc.FloatingIPs {
		alloc, _ := time.Parse(time.RFC3339Nano, b.AllocatedAt)
		f := FloatingIP{
			UUID:        b.UUID,
			ProjectUUID: b.ProjectUUID,
			NetworkUUID: b.NetworkUUID,
			Address:     b.Address,
			MappedTo:    b.MappedTo,
			TargetKind:  FloatingIPTargetKind(b.TargetKind),
			Status:      FloatingIPStatus(b.Status),
			AllocatedAt: alloc,
		}
		reg.indexLocked(f)
	}
	return reg, nil
}

func newFloatingIPRegistry(storage Storage) *floatingIPRegistry {
	return &floatingIPRegistry{
		storage:    storage,
		byUUID:     make(map[string]FloatingIP),
		addrIdx:    make(map[string]string),
		projectIdx: make(map[string]map[string]struct{}),
		targetIdx:  make(map[string]map[string]struct{}),
	}
}

// addrKey computes the (network, address) composite for addrIdx.
func addrKey(networkUUID, address string) string { return networkUUID + "\x00" + address }

// targetKey computes the (kind, target) composite for targetIdx.
func targetKey(kind FloatingIPTargetKind, target string) string {
	return string(kind) + "\x00" + target
}

// indexLocked inserts f into every index. Caller holds mu (or is
// in a single-threaded loader). Pure update — never blocks on IO.
func (r *floatingIPRegistry) indexLocked(f FloatingIP) {
	r.byUUID[f.UUID] = f
	r.addrIdx[addrKey(f.NetworkUUID, f.Address)] = f.UUID
	if _, ok := r.projectIdx[f.ProjectUUID]; !ok {
		r.projectIdx[f.ProjectUUID] = make(map[string]struct{})
	}
	r.projectIdx[f.ProjectUUID][f.UUID] = struct{}{}
	if f.MappedTo != "" {
		k := targetKey(f.TargetKind, f.MappedTo)
		if _, ok := r.targetIdx[k]; !ok {
			r.targetIdx[k] = make(map[string]struct{})
		}
		r.targetIdx[k][f.UUID] = struct{}{}
	}
}

// unindexLocked is the inverse — removes f from every index.
func (r *floatingIPRegistry) unindexLocked(f FloatingIP) {
	delete(r.byUUID, f.UUID)
	delete(r.addrIdx, addrKey(f.NetworkUUID, f.Address))
	if set, ok := r.projectIdx[f.ProjectUUID]; ok {
		delete(set, f.UUID)
		if len(set) == 0 {
			delete(r.projectIdx, f.ProjectUUID)
		}
	}
	if f.MappedTo != "" {
		k := targetKey(f.TargetKind, f.MappedTo)
		if set, ok := r.targetIdx[k]; ok {
			delete(set, f.UUID)
			if len(set) == 0 {
				delete(r.targetIdx, k)
			}
		}
	}
}

// saveLocked writes via Storage. Caller holds mu.
func (r *floatingIPRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft floating-ip registry — UUID-keyed per [[weft-uuid-keyed-resources]].\n" +
				"# Lifecycle : Allocate → Map ⇄ Unmap → Release. Edit `mapped_to`,\n" +
				"# `target_kind`, `status` via the gRPC API ; never change the\n" +
				"# floating_ip label (UUID), `project_uuid`, `network_uuid`, or\n" +
				"# `address` once allocated.\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		fip := r.byUUID[u]
		block := body.AppendNewBlock("floating_ip", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("project_uuid", cty.StringVal(fip.ProjectUUID))
		bb.SetAttributeValue("network_uuid", cty.StringVal(fip.NetworkUUID))
		bb.SetAttributeValue("address", cty.StringVal(fip.Address))
		if fip.MappedTo != "" {
			bb.SetAttributeValue("mapped_to", cty.StringVal(fip.MappedTo))
		}
		if fip.TargetKind != "" {
			bb.SetAttributeValue("target_kind", cty.StringVal(string(fip.TargetKind)))
		}
		bb.SetAttributeValue("status", cty.StringVal(string(fip.Status)))
		bb.SetAttributeValue("allocated_at", cty.StringVal(fip.AllocatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// lookupByUUID returns (FloatingIP, true) when known.
func (r *floatingIPRegistry) lookupByUUID(uuid string) (FloatingIP, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fip, ok := r.byUUID[uuid]
	return fip, ok
}

// listForProject returns every FIP owned by project, sorted by
// address (for stable diff output in the CLI and UI).
func (r *floatingIPRegistry) listForProject(projectUUID string) []FloatingIP {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.projectIdx[projectUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]FloatingIP, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sortFloatingIPs(out)
	return out
}

// list returns every FIP across all projects, sorted by
// (ProjectUUID, Address).
func (r *floatingIPRegistry) list() []FloatingIP {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]FloatingIP, 0, len(r.byUUID))
	for _, fip := range r.byUUID {
		out = append(out, fip)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// listForTarget returns every FIP currently mapped to (kind, name).
// Empty list = no mappings (the answer the reconciler needs to
// install zero NAT rules).
func (r *floatingIPRegistry) listForTarget(kind FloatingIPTargetKind, target string) []FloatingIP {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.targetIdx[targetKey(kind, target)]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]FloatingIP, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sortFloatingIPs(out)
	return out
}

func sortFloatingIPs(out []FloatingIP) {
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
}

// AllocateFloatingIPSpec carries the inputs Allocate validates.
type AllocateFloatingIPSpec struct {
	ProjectUUID string
	NetworkUUID string
	// Address, when non-empty, asks for a specific address from
	// the network's CIDR (must be free). When empty, the registry
	// picks the next available. Useful for re-allocation /
	// migration scenarios where the operator wants to keep an
	// address stable.
	Address string
	// PortInUse is the caller's snapshot of every port-occupied
	// address on the network, so the registry skips them when
	// auto-allocating. Empty = no constraint.
	PortInUse []string
	// Reserved holds the addresses the network reserves
	// internally — gateway, broadcast, DHCP server, etc. — that
	// must never be handed out. Empty = no constraint.
	Reserved []string
}

// allocate registers a new FIP. Returns the persisted entry.
// Caller (Adapter.AllocateFloatingIP) has already validated the
// project / network existence.
func (r *floatingIPRegistry) allocate(spec AllocateFloatingIPSpec, cidr string) (FloatingIP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	addr := spec.Address
	if addr == "" {
		picked, err := r.pickFreeAddressLocked(spec.NetworkUUID, cidr, spec.PortInUse, spec.Reserved)
		if err != nil {
			return FloatingIP{}, err
		}
		addr = picked
	} else {
		if err := validateAddressLocked(r, spec.NetworkUUID, cidr, addr, spec.PortInUse, spec.Reserved); err != nil {
			return FloatingIP{}, err
		}
	}

	uuid, err := randomUUID()
	if err != nil {
		return FloatingIP{}, fmt.Errorf("generate uuid: %w", err)
	}
	fip := FloatingIP{
		UUID:        uuid,
		ProjectUUID: spec.ProjectUUID,
		NetworkUUID: spec.NetworkUUID,
		Address:     addr,
		Status:      FIPStatusAvailable,
		AllocatedAt: time.Now().UTC(),
	}
	r.indexLocked(fip)
	if err := r.saveLocked(); err != nil {
		r.unindexLocked(fip)
		return FloatingIP{}, fmt.Errorf("persist: %w", err)
	}
	return fip, nil
}

// pickFreeAddressLocked walks the CIDR and returns the first IP
// not present in addrIdx, PortInUse, or Reserved. The first/last
// host addresses of an IPv4 /n network (network + broadcast) are
// skipped automatically — they're never valid hosts.
func (r *floatingIPRegistry) pickFreeAddressLocked(networkUUID, cidr string, portInUse, reserved []string) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	excluded := makeAddrSet(portInUse, reserved)
	ip := prefix.Masked().Addr()
	first := true
	for prefix.Contains(ip) {
		// Skip the network address (first) and broadcast (last,
		// IPv4 only) — never valid hosts.
		if !first && !isIPv4Broadcast(ip, prefix) {
			s := ip.String()
			if _, taken := excluded[s]; !taken {
				if _, used := r.addrIdx[addrKey(networkUUID, s)]; !used {
					return s, nil
				}
			}
		}
		first = false
		ip = ip.Next()
		if !ip.IsValid() {
			break
		}
	}
	return "", fmt.Errorf("no free addresses in %s", cidr)
}

// validateAddressLocked checks an operator-supplied address is in
// the CIDR, not already in the registry, not port-occupied, not
// reserved, and not a network/broadcast address.
func validateAddressLocked(r *floatingIPRegistry, networkUUID, cidr, address string, portInUse, reserved []string) error {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return fmt.Errorf("parse cidr %q: %w", cidr, err)
	}
	addr, err := netip.ParseAddr(address)
	if err != nil {
		return fmt.Errorf("parse address %q: %w", address, err)
	}
	if !prefix.Contains(addr) {
		return fmt.Errorf("address %s is not in network %s", address, cidr)
	}
	if addr == prefix.Masked().Addr() {
		return fmt.Errorf("address %s is the network address of %s", address, cidr)
	}
	if isIPv4Broadcast(addr, prefix) {
		return fmt.Errorf("address %s is the broadcast address of %s", address, cidr)
	}
	if _, used := r.addrIdx[addrKey(networkUUID, address)]; used {
		return fmt.Errorf("address %s already allocated on network %s", address, networkUUID)
	}
	excluded := makeAddrSet(portInUse, reserved)
	if _, taken := excluded[address]; taken {
		return fmt.Errorf("address %s is taken (port-occupied or reserved)", address)
	}
	return nil
}

// makeAddrSet flattens two lists into one set keyed by the
// canonical netip string form (so "10.0.0.1" and "10.0.0.01" don't
// hash differently if one of them ever slips through).
func makeAddrSet(lists ...[]string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, list := range lists {
		for _, s := range list {
			if addr, err := netip.ParseAddr(s); err == nil {
				out[addr.String()] = struct{}{}
			} else {
				// Fall back to the raw string if it doesn't
				// parse — better to over-reject than to leak.
				out[s] = struct{}{}
			}
		}
	}
	return out
}

// isIPv4Broadcast returns true when addr is the broadcast (all-1s
// host) address of prefix. Always false for IPv6.
func isIPv4Broadcast(addr netip.Addr, prefix netip.Prefix) bool {
	if !addr.Is4() || !prefix.Addr().Is4() {
		return false
	}
	// Compute the broadcast as network-or-host-mask.
	masked := prefix.Masked().Addr().As4()
	bits := prefix.Bits()
	if bits >= 32 {
		return false // /32 has no broadcast — single host
	}
	hostBits := 32 - bits
	var bcast [4]byte
	for i := 0; i < 4; i++ {
		bcast[i] = masked[i]
	}
	// Set the trailing hostBits bits to 1.
	for i := 31; i >= 32-hostBits; i-- {
		byteIdx, bitIdx := i/8, uint(7-i%8)
		bcast[byteIdx] |= 1 << bitIdx
	}
	return netip.AddrFrom4(bcast) == addr
}

// release removes fip from the registry. Idempotent on missing.
func (r *floatingIPRegistry) release(uuid string) (FloatingIP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fip, ok := r.byUUID[uuid]
	if !ok {
		return FloatingIP{}, fmt.Errorf("floating ip %q not found", uuid)
	}
	if fip.Status == FIPStatusActive {
		return FloatingIP{}, fmt.Errorf("floating ip %q is active (mapped to %s %q) — unmap before releasing",
			uuid, fip.TargetKind, fip.MappedTo)
	}
	r.unindexLocked(fip)
	if err := r.saveLocked(); err != nil {
		r.indexLocked(fip)
		return FloatingIP{}, fmt.Errorf("persist: %w", err)
	}
	return fip, nil
}

// mapTo binds fip to (kind, target). Idempotent : a no-op when
// already at that state ; an error when bound to a different
// target (caller must Unmap first to make the intent explicit).
func (r *floatingIPRegistry) mapTo(uuid string, kind FloatingIPTargetKind, target string) (FloatingIP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fip, ok := r.byUUID[uuid]
	if !ok {
		return FloatingIP{}, fmt.Errorf("floating ip %q not found", uuid)
	}
	if fip.Status == FIPStatusActive {
		if fip.TargetKind == kind && fip.MappedTo == target {
			return fip, nil // already there — idempotent
		}
		return FloatingIP{}, fmt.Errorf("floating ip %q already mapped to %s %q ; unmap first",
			uuid, fip.TargetKind, fip.MappedTo)
	}
	old := fip
	fip.MappedTo = target
	fip.TargetKind = kind
	fip.Status = FIPStatusActive
	r.unindexLocked(old)
	r.indexLocked(fip)
	if err := r.saveLocked(); err != nil {
		r.unindexLocked(fip)
		r.indexLocked(old)
		return FloatingIP{}, fmt.Errorf("persist: %w", err)
	}
	return fip, nil
}

// unmap clears the binding on fip. Idempotent on already-unmapped.
func (r *floatingIPRegistry) unmap(uuid string) (FloatingIP, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fip, ok := r.byUUID[uuid]
	if !ok {
		return FloatingIP{}, fmt.Errorf("floating ip %q not found", uuid)
	}
	if fip.Status != FIPStatusActive {
		return fip, nil // already available — idempotent
	}
	old := fip
	fip.MappedTo = ""
	fip.TargetKind = ""
	fip.Status = FIPStatusAvailable
	r.unindexLocked(old)
	r.indexLocked(fip)
	if err := r.saveLocked(); err != nil {
		r.unindexLocked(fip)
		r.indexLocked(old)
		return FloatingIP{}, fmt.Errorf("persist: %w", err)
	}
	return fip, nil
}

// randomUUID returns a 16-byte random hex string. Local impl so
// the package doesn't grow another dep just for this one need ;
// the rest of weft uses the same shape (see UUID helpers
// scattered across registries).
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// helper for tests : extract the just-allocated addresses on a
// network. Not part of the public surface — only exercised by
// the registry's own tests.
func (r *floatingIPRegistry) addressesOn(networkUUID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	for k := range r.addrIdx {
		if len(k) > len(networkUUID)+1 && k[:len(networkUUID)] == networkUUID && k[len(networkUUID)] == 0 {
			out = append(out, k[len(networkUUID)+1:])
		}
	}
	sort.Strings(out)
	return out
}

// netParseCIDR is a backwards-friendly wrapper around net.ParseCIDR
// for callers that prefer the older shape. Not used internally —
// netip.ParsePrefix is the canonical path — but kept available
// for adapter-level glue that still receives net.IPNet.
func netParseCIDR(s string) (*net.IPNet, error) {
	_, ipn, err := net.ParseCIDR(s)
	return ipn, err
}
