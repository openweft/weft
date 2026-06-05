package weft

// dnsrecords.go owns the DNSRecord registry — zone children. Each
// record carries (name, type, value, ttl, priority). The Adapter
// wrapper supplies the records-count cascade the zone delete checks.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// DNSRecord is one entry in the registry.
type DNSRecord struct {
	UUID      string    `json:"uuid"`
	ZoneUUID  string    `json:"zone_uuid"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Value     string    `json:"value"`
	TTL       int32     `json:"ttl,omitempty"`
	Priority  int32     `json:"priority,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type dnsRecordsDoc struct {
	Records []DNSRecord `json:"records"`
}

const dnsRecordRegistryFileName = "dns_records"

type dnsRecordRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]DNSRecord
	zoneIdx map[string]map[string]struct{} // zoneUUID → set(recordUUID)
}

func loadDNSRecordRegistry(ctx context.Context, storage Storage) (*dnsRecordRegistry, error) {
	reg := &dnsRecordRegistry{
		storage: storage,
		byUUID:  make(map[string]DNSRecord),
		zoneIdx: make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load dns record registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc dnsRecordsDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse dns record registry: %w", err)
	}
	for _, rec := range doc.Records {
		reg.byUUID[rec.UUID] = rec
		reg.indexLocked(rec)
	}
	return reg, nil
}

func (r *dnsRecordRegistry) indexLocked(rec DNSRecord) {
	if _, ok := r.zoneIdx[rec.ZoneUUID]; !ok {
		r.zoneIdx[rec.ZoneUUID] = make(map[string]struct{})
	}
	r.zoneIdx[rec.ZoneUUID][rec.UUID] = struct{}{}
}

func (r *dnsRecordRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := dnsRecordsDoc{Records: make([]DNSRecord, 0, len(uuids))}
	for _, u := range uuids {
		doc.Records = append(doc.Records, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode dns record registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save dns record registry: %w", err)
	}
	return nil
}

func (r *dnsRecordRegistry) lookupByUUID(uuid string) (DNSRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byUUID[uuid]
	return rec, ok
}

// countForZone returns the live record count for a zone. Cheap (the
// in-memory index is one map lookup) ; OK to call from the cascade
// check on every DeleteDNSZone.
func (r *dnsRecordRegistry) countForZone(zoneUUID string) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int32(len(r.zoneIdx[zoneUUID]))
}

func (r *dnsRecordRegistry) list(zoneUUID string) []DNSRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DNSRecord, 0, len(r.byUUID))
	for _, rec := range r.byUUID {
		if zoneUUID != "" && rec.ZoneUUID != zoneUUID {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// validDNSTypes enumerates accepted record types.
var validDNSTypes = map[string]struct{}{
	"A": {}, "AAAA": {}, "CNAME": {}, "MX": {}, "TXT": {}, "SRV": {},
}

func (r *dnsRecordRegistry) create(zoneUUID, name, recordType, value string, ttl, priority int32) (DNSRecord, bool, error) {
	if zoneUUID == "" {
		return DNSRecord{}, false, fmt.Errorf("zone_uuid is required")
	}
	if name == "" {
		return DNSRecord{}, false, fmt.Errorf("name is required")
	}
	if _, ok := validDNSTypes[recordType]; !ok {
		return DNSRecord{}, false, fmt.Errorf("type %q must be one of A/AAAA/CNAME/MX/TXT/SRV", recordType)
	}
	if value == "" {
		return DNSRecord{}, false, fmt.Errorf("value is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Idempotent on (zone, name, type, value) so a retry is a no-op.
	for _, rec := range r.byUUID {
		if rec.ZoneUUID == zoneUUID && rec.Name == name && rec.Type == recordType && rec.Value == value {
			return rec, false, nil
		}
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return DNSRecord{}, false, err
	}
	rec := DNSRecord{
		UUID:      uuid,
		ZoneUUID:  zoneUUID,
		Name:      name,
		Type:      recordType,
		Value:     value,
		TTL:       ttl,
		Priority:  priority,
		CreatedAt: time.Now().UTC(),
	}
	r.byUUID[uuid] = rec
	r.indexLocked(rec)
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.zoneIdx[zoneUUID], uuid)
		return DNSRecord{}, false, err
	}
	return rec, true, nil
}

// update patches mutable fields. ttl == -1, priority == -1 = keep current.
func (r *dnsRecordRegistry) update(uuid, value string, ttl, priority int32) (DNSRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return DNSRecord{}, fmt.Errorf("dns record %s not found", uuid)
	}
	if value != "" {
		cur.Value = value
	}
	if ttl >= 0 {
		cur.TTL = ttl
	}
	if priority >= 0 {
		cur.Priority = priority
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return DNSRecord{}, err
	}
	return cur, nil
}

func (r *dnsRecordRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("dns record %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	if set, ok := r.zoneIdx[cur.ZoneUUID]; ok {
		delete(set, uuid)
		if len(set) == 0 {
			delete(r.zoneIdx, cur.ZoneUUID)
		}
	}
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.indexLocked(cur)
		return err
	}
	return nil
}
