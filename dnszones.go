package weft

// dnszones.go owns the DNSZone registry — per-project authoritative
// apex (FQDN like "acme.example.com"). DNSRecords are children in
// dnsrecords.go ; cascade between the two lives in the Adapter
// wrapper (DeleteDNSZone refuses while records still attach).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DNSZone is one entry in the registry.
type DNSZone struct {
	UUID        string    `json:"uuid"`
	ProjectUUID string    `json:"project_uuid"`
	Name        string    `json:"name"`
	SOAEmail    string    `json:"soa_email,omitempty"`
	TTL         int32     `json:"ttl,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type dnsZonesDoc struct {
	Zones []DNSZone `json:"zones"`
}

const dnsZoneRegistryFileName = "dns_zones"

type dnsZoneRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]DNSZone
	nameIdx map[string]string // FQDN → uuid (globally unique)
}

func loadDNSZoneRegistry(ctx context.Context, storage Storage) (*dnsZoneRegistry, error) {
	reg := &dnsZoneRegistry{
		storage: storage,
		byUUID:  make(map[string]DNSZone),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load dns zone registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc dnsZonesDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse dns zone registry: %w", err)
	}
	for _, z := range doc.Zones {
		reg.byUUID[z.UUID] = z
		reg.nameIdx[z.Name] = z.UUID
	}
	return reg, nil
}

func (r *dnsZoneRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := dnsZonesDoc{Zones: make([]DNSZone, 0, len(uuids))}
	for _, u := range uuids {
		doc.Zones = append(doc.Zones, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dns zone registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save dns zone registry: %w", err)
	}
	return nil
}

func (r *dnsZoneRegistry) lookupByUUID(uuid string) (DNSZone, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	z, ok := r.byUUID[uuid]
	return z, ok
}

func (r *dnsZoneRegistry) lookupByName(name string) (DNSZone, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[name]
	if !ok {
		return DNSZone{}, false
	}
	return r.byUUID[uuid], true
}

func (r *dnsZoneRegistry) list(projectUUID string) []DNSZone {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DNSZone, 0, len(r.byUUID))
	for _, z := range r.byUUID {
		if projectUUID != "" && z.ProjectUUID != projectUUID {
			continue
		}
		out = append(out, z)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *dnsZoneRegistry) create(projectUUID, name, soaEmail string, ttl int32) (DNSZone, bool, error) {
	if name == "" {
		return DNSZone{}, false, fmt.Errorf("name is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[name]; ok {
		return r.byUUID[uuid], false, nil
	}
	if ttl <= 0 {
		ttl = 3600
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return DNSZone{}, false, err
	}
	z := DNSZone{
		UUID:        uuid,
		ProjectUUID: projectUUID,
		Name:        name,
		SOAEmail:    soaEmail,
		TTL:         ttl,
		CreatedAt:   time.Now().UTC(),
	}
	r.byUUID[uuid] = z
	r.nameIdx[name] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, name)
		return DNSZone{}, false, err
	}
	return z, true, nil
}

// update patches the mutable fields. ttl == -1 means "keep current".
func (r *dnsZoneRegistry) update(uuid, soaEmail string, ttl int32) (DNSZone, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return DNSZone{}, fmt.Errorf("dns zone %s not found", uuid)
	}
	if soaEmail != "" {
		cur.SOAEmail = soaEmail
	}
	if ttl >= 0 {
		cur.TTL = ttl
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return DNSZone{}, err
	}
	return cur, nil
}

func (r *dnsZoneRegistry) delete(uuid string, blockedByRecords int32) (int32, error) {
	if blockedByRecords > 0 {
		return blockedByRecords, fmt.Errorf("dns zone %s still has %d record(s) — drain first", uuid, blockedByRecords)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return 0, fmt.Errorf("dns zone %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, cur.Name)
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.nameIdx[cur.Name] = uuid
		return 0, err
	}
	return 0, nil
}
