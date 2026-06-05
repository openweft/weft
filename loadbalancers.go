package weft

// loadbalancers.go owns the LoadBalancer registry — project-
// scoped VIPs with L4 / L7 protocols and an atomically-replaceable
// backend pool. The proto's `SetLoadBalancerBackends` mirrors
// `SetSecurityGroupRules` : the wire never carries single-entry
// add/remove ; clients GET-modify-PUT.
//
// Cascade safety : DeleteLoadBalancer refuses when a FloatingIP
// still maps to the VIP. The Adapter wrapper supplies the
// `blocked_by_fips` count (it consults the FIP registry which the
// LB registry doesn't know about, by design).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// LBBackend is one entry in a LoadBalancer's backend pool.
type LBBackend struct {
	Address string `json:"address"`
	Weight  int32  `json:"weight"`
}

// LoadBalancer is one entry in the registry.
type LoadBalancer struct {
	UUID        string      `json:"uuid"`
	ProjectUUID string      `json:"project_uuid"`
	Name        string      `json:"name"`
	ListenAddr  string      `json:"listen_addr"`
	Protocol    string      `json:"protocol"`
	Backends    []LBBackend `json:"backends,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
}

type loadBalancersDoc struct {
	LoadBalancers []LoadBalancer `json:"load_balancers"`
}

const loadBalancerRegistryFileName = "loadbalancers"

type loadBalancerRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]LoadBalancer
	nameIdx map[string]string // (projectUUID + "\x00" + name) → uuid
}

func loadLoadBalancerRegistry(ctx context.Context, storage Storage) (*loadBalancerRegistry, error) {
	reg := &loadBalancerRegistry{
		storage: storage,
		byUUID:  make(map[string]LoadBalancer),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load loadbalancer registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc loadBalancersDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse loadbalancer registry: %w", err)
	}
	for _, lb := range doc.LoadBalancers {
		reg.byUUID[lb.UUID] = lb
		reg.nameIdx[lbNameKey(lb.ProjectUUID, lb.Name)] = lb.UUID
	}
	return reg, nil
}

func lbNameKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
}

func (r *loadBalancerRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := loadBalancersDoc{LoadBalancers: make([]LoadBalancer, 0, len(uuids))}
	for _, u := range uuids {
		doc.LoadBalancers = append(doc.LoadBalancers, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode loadbalancer registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save loadbalancer registry: %w", err)
	}
	return nil
}

func (r *loadBalancerRegistry) lookupByUUID(uuid string) (LoadBalancer, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lb, ok := r.byUUID[uuid]
	return lb, ok
}

// list returns every LB ; projectUUID filters to one project (empty = all).
func (r *loadBalancerRegistry) list(projectUUID string) []LoadBalancer {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LoadBalancer, 0, len(r.byUUID))
	for _, lb := range r.byUUID {
		if projectUUID != "" && lb.ProjectUUID != projectUUID {
			continue
		}
		out = append(out, lb)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// validProtocols enumerates accepted LoadBalancer protocols.
var validLBProtocols = map[string]struct{}{
	"l4_tcp":  {},
	"l4_udp":  {},
	"l7_http": {},
	"l7_https": {},
}

func (r *loadBalancerRegistry) create(projectUUID, name, listenAddr, protocol string, backends []LBBackend) (LoadBalancer, bool, error) {
	if projectUUID == "" {
		return LoadBalancer{}, false, fmt.Errorf("project_uuid is required")
	}
	if name == "" {
		return LoadBalancer{}, false, fmt.Errorf("name is required")
	}
	if listenAddr == "" {
		return LoadBalancer{}, false, fmt.Errorf("listen_addr is required")
	}
	if _, ok := validLBProtocols[protocol]; !ok {
		return LoadBalancer{}, false, fmt.Errorf("protocol %q must be one of l4_tcp/l4_udp/l7_http/l7_https", protocol)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[lbNameKey(projectUUID, name)]; ok {
		return r.byUUID[uuid], false, nil
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return LoadBalancer{}, false, err
	}
	lb := LoadBalancer{
		UUID:        uuid,
		ProjectUUID: projectUUID,
		Name:        name,
		ListenAddr:  listenAddr,
		Protocol:    protocol,
		Backends:    append([]LBBackend(nil), backends...),
		CreatedAt:   time.Now().UTC(),
	}
	r.byUUID[uuid] = lb
	r.nameIdx[lbNameKey(projectUUID, name)] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, lbNameKey(projectUUID, name))
		return LoadBalancer{}, false, err
	}
	return lb, true, nil
}

func (r *loadBalancerRegistry) update(uuid, name, listenAddr, protocol string) (LoadBalancer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return LoadBalancer{}, fmt.Errorf("loadbalancer %s not found", uuid)
	}
	if name != "" && name != cur.Name {
		key := lbNameKey(cur.ProjectUUID, name)
		if other, taken := r.nameIdx[key]; taken && other != uuid {
			return LoadBalancer{}, fmt.Errorf("loadbalancer name %q already used in project", name)
		}
		delete(r.nameIdx, lbNameKey(cur.ProjectUUID, cur.Name))
		cur.Name = name
		r.nameIdx[key] = uuid
	}
	if listenAddr != "" {
		cur.ListenAddr = listenAddr
	}
	if protocol != "" {
		if _, ok := validLBProtocols[protocol]; !ok {
			return LoadBalancer{}, fmt.Errorf("protocol %q must be one of l4_tcp/l4_udp/l7_http/l7_https", protocol)
		}
		cur.Protocol = protocol
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return LoadBalancer{}, err
	}
	return cur, nil
}

// setBackends replaces the backend list atomically.
func (r *loadBalancerRegistry) setBackends(uuid string, backends []LBBackend) (LoadBalancer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return LoadBalancer{}, fmt.Errorf("loadbalancer %s not found", uuid)
	}
	cur.Backends = append([]LBBackend(nil), backends...)
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return LoadBalancer{}, err
	}
	return cur, nil
}

func (r *loadBalancerRegistry) delete(uuid string, blockedByFIPs int32) (int32, error) {
	if blockedByFIPs > 0 {
		return blockedByFIPs, fmt.Errorf("loadbalancer %s still mapped by %d floating-ip(s) — unmap first", uuid, blockedByFIPs)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return 0, fmt.Errorf("loadbalancer %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, lbNameKey(cur.ProjectUUID, cur.Name))
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.nameIdx[lbNameKey(cur.ProjectUUID, cur.Name)] = uuid
		return 0, err
	}
	return 0, nil
}
