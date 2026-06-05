package weft

// subnets.go owns the Subnet registry — per-network IP scopes. A
// Network can carry multiple Subnets (dual-stack v4+v6, multi-vlan,
// staged rollouts). The proto's `cidr` is immutable ; `gateway` /
// `dns_servers` / display fields move via UpdateSubnet.
//
// Persistence : JSON document via the Storage interface, same as
// the AZ/Rack registries. Cascade safety lives in the Adapter
// wrapper (DeleteSubnet refuses when ports still allocate from the
// scope — once weft-network's port registry is wired ; today the
// cascade hook is a TODO surface that returns 0 so the operator
// can still drain by hand).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Subnet is one entry in the registry.
type Subnet struct {
	UUID        string    `json:"uuid"`
	NetworkUUID string    `json:"network_uuid"`
	ProjectUUID string    `json:"project_uuid"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	CIDR        string    `json:"cidr"`
	Gateway     string    `json:"gateway,omitempty"`
	DNSServers  []string  `json:"dns_servers,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type subnetsDoc struct {
	Subnets []Subnet `json:"subnets"`
}

const subnetRegistryFileName = "subnets"

type subnetRegistry struct {
	mu         sync.Mutex
	storage    Storage
	byUUID     map[string]Subnet
	networkIdx map[string]map[string]struct{} // networkUUID → set(subnetUUID)
}

func loadSubnetRegistry(ctx context.Context, storage Storage) (*subnetRegistry, error) {
	reg := &subnetRegistry{
		storage:    storage,
		byUUID:     make(map[string]Subnet),
		networkIdx: make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load subnet registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc subnetsDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse subnet registry: %w", err)
	}
	for _, s := range doc.Subnets {
		reg.byUUID[s.UUID] = s
		reg.indexLocked(s)
	}
	return reg, nil
}

func (r *subnetRegistry) indexLocked(s Subnet) {
	if _, ok := r.networkIdx[s.NetworkUUID]; !ok {
		r.networkIdx[s.NetworkUUID] = make(map[string]struct{})
	}
	r.networkIdx[s.NetworkUUID][s.UUID] = struct{}{}
}

func (r *subnetRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := subnetsDoc{Subnets: make([]Subnet, 0, len(uuids))}
	for _, u := range uuids {
		doc.Subnets = append(doc.Subnets, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode subnet registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save subnet registry: %w", err)
	}
	return nil
}

func (r *subnetRegistry) lookupByUUID(uuid string) (Subnet, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byUUID[uuid]
	return s, ok
}

// list returns every subnet. `networkUUID` filters to one parent ;
// empty returns the full set.
func (r *subnetRegistry) list(networkUUID string) []Subnet {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Subnet, 0, len(r.byUUID))
	for _, s := range r.byUUID {
		if networkUUID != "" && s.NetworkUUID != networkUUID {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NetworkUUID != out[j].NetworkUUID {
			return out[i].NetworkUUID < out[j].NetworkUUID
		}
		return out[i].CIDR < out[j].CIDR
	})
	return out
}

func (r *subnetRegistry) create(networkUUID, projectUUID, name, description, cidr, gateway string, dnsServers []string) (Subnet, bool, error) {
	if networkUUID == "" {
		return Subnet{}, false, fmt.Errorf("network_uuid is required")
	}
	if cidr == "" {
		return Subnet{}, false, fmt.Errorf("cidr is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Idempotent on (network, cidr).
	for _, s := range r.byUUID {
		if s.NetworkUUID == networkUUID && s.CIDR == cidr {
			return s, false, nil
		}
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return Subnet{}, false, err
	}
	s := Subnet{
		UUID:        uuid,
		NetworkUUID: networkUUID,
		ProjectUUID: projectUUID,
		Name:        name,
		Description: description,
		CIDR:        cidr,
		Gateway:     gateway,
		DNSServers:  append([]string(nil), dnsServers...),
		CreatedAt:   time.Now().UTC(),
	}
	r.byUUID[uuid] = s
	r.indexLocked(s)
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.networkIdx[networkUUID], uuid)
		return Subnet{}, false, err
	}
	return s, true, nil
}

// update patches the mutable fields. Empty string = keep current.
// clearDNS=true with empty dnsServers means "clear the list" ;
// dnsServers non-nil replaces ; nil + clearDNS=false = keep current.
func (r *subnetRegistry) update(uuid, name, description, gateway string, clearDNS bool, dnsServers []string) (Subnet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return Subnet{}, fmt.Errorf("subnet %s not found", uuid)
	}
	if name != "" {
		cur.Name = name
	}
	if description != "" {
		cur.Description = description
	}
	if gateway != "" {
		cur.Gateway = gateway
	}
	switch {
	case clearDNS:
		cur.DNSServers = nil
	case len(dnsServers) > 0:
		cur.DNSServers = append([]string(nil), dnsServers...)
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return Subnet{}, err
	}
	return cur, nil
}

func (r *subnetRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("subnet %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	if set, ok := r.networkIdx[cur.NetworkUUID]; ok {
		delete(set, uuid)
		if len(set) == 0 {
			delete(r.networkIdx, cur.NetworkUUID)
		}
	}
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.indexLocked(cur)
		return err
	}
	return nil
}
