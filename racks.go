package weft

// racks.go owns the Rack registry — the second tier of the
// inventory hierarchy. Every rack pins to a parent AZ via az_uuid ;
// the registry refuses inserts whose parent AZ doesn't exist
// (referential integrity, not just a foreign-key warning).
//
// HeightU is the rack's total U capacity ; the webui's 2D
// rack-elevation viz draws hosts at their host.PositionU slot up
// to height_u. Scheduler placement honours az_code + rack_code but
// is otherwise blind to U-occupancy.
//
// Persistence + concurrency mirror azs.go's pattern : JSON document
// under Storage, atomic save on every mutation, in-memory cache
// keyed by UUID with a (az_uuid, code) secondary index.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Rack is one entry in the registry.
type Rack struct {
	UUID      string    `json:"uuid"`
	AZUUID    string    `json:"az_uuid"`
	Code      string    `json:"code"` // short id, e.g. "R1"
	Name      string    `json:"name"`
	Status    string    `json:"status"`   // "active" | "draining" | "down"
	HeightU   int32     `json:"height_u"` // total U capacity, 0 = unspecified
	CreatedAt time.Time `json:"created_at"`
}

// racksDoc is the JSON top-level document.
type racksDoc struct {
	Racks []Rack `json:"racks"`
}

// rackRegistryFileName is the conventional file basename.
const rackRegistryFileName = "racks"

// rackKey is the (az_uuid, code) tuple used in the secondary
// uniqueness index — the same rack code can repeat across AZs but
// must be unique within one.
type rackKey struct {
	AZUUID string
	Code   string
}

// rackRegistry is the in-memory cache backed by a Storage.
type rackRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]Rack
	byKey   map[rackKey]string // (az_uuid, code) → uuid
}

// loadRackRegistry reads the blob via Storage. Empty blob → empty
// registry.
func loadRackRegistry(ctx context.Context, storage Storage) (*rackRegistry, error) {
	reg := &rackRegistry{
		storage: storage,
		byUUID:  make(map[string]Rack),
		byKey:   make(map[rackKey]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load rack registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc racksDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse rack registry: %w", err)
	}
	for _, r2 := range doc.Racks {
		reg.byUUID[r2.UUID] = r2
		reg.byKey[rackKey{AZUUID: r2.AZUUID, Code: r2.Code}] = r2.UUID
	}
	return reg, nil
}

// saveLocked serialises the cache. Caller holds mu. Output sorted
// by (az_uuid, code) for stable diffs.
func (r *rackRegistry) saveLocked() error {
	keys := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		keys = append(keys, u)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := r.byUUID[keys[i]], r.byUUID[keys[j]]
		if ri.AZUUID != rj.AZUUID {
			return ri.AZUUID < rj.AZUUID
		}
		return ri.Code < rj.Code
	})
	doc := racksDoc{Racks: make([]Rack, 0, len(keys))}
	for _, u := range keys {
		doc.Racks = append(doc.Racks, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode rack registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save rack registry: %w", err)
	}
	return nil
}

// lookupByUUID returns the Rack + a found flag.
func (r *rackRegistry) lookupByUUID(uuid string) (Rack, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rk, ok := r.byUUID[uuid]
	return rk, ok
}

// list returns every rack ; az_uuid empty filters to all AZs.
func (r *rackRegistry) list(azUUID string) []Rack {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Rack, 0, len(r.byUUID))
	for _, rk := range r.byUUID {
		if azUUID != "" && rk.AZUUID != azUUID {
			continue
		}
		out = append(out, rk)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AZUUID != out[j].AZUUID {
			return out[i].AZUUID < out[j].AZUUID
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// countForAZ returns how many racks bind to azUUID. Used by the AZ
// delete path's cascade safety check.
func (r *rackRegistry) countForAZ(azUUID string) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int32
	for _, rk := range r.byUUID {
		if rk.AZUUID == azUUID {
			n++
		}
	}
	return n
}

// create registers a new rack. Returns (row, true) on insert,
// (existing, false) when an (az_uuid, code) tuple already lives.
// azExists is supplied by the caller (Adapter) so the registry
// doesn't depend on the AZ registry struct directly.
func (r *rackRegistry) create(azUUID, code, name, statusValue string, heightU int32, azExists bool) (Rack, bool, error) {
	if azUUID == "" {
		return Rack{}, false, fmt.Errorf("rack az_uuid must not be empty")
	}
	if !azExists {
		return Rack{}, false, fmt.Errorf("az %s not found", azUUID)
	}
	if code == "" {
		return Rack{}, false, fmt.Errorf("rack code must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.byKey[rackKey{AZUUID: azUUID, Code: code}]; ok {
		return r.byUUID[uuid], false, nil
	}
	if statusValue == "" {
		statusValue = "active"
	}
	uuid, err := mintInventoryUUID()
	if err != nil {
		return Rack{}, false, err
	}
	rk := Rack{
		UUID:      uuid,
		AZUUID:    azUUID,
		Code:      code,
		Name:      name,
		Status:    statusValue,
		HeightU:   heightU,
		CreatedAt: time.Now().UTC(),
	}
	r.byUUID[uuid] = rk
	r.byKey[rackKey{AZUUID: azUUID, Code: code}] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.byKey, rackKey{AZUUID: azUUID, Code: code})
		return Rack{}, false, err
	}
	return rk, true, nil
}

// update patches the mutable fields. Empty-string args = keep
// current. heightU == -1 = keep current (proto3 scalars have no
// nil so the wire layer encodes "absent" as -1).
func (r *rackRegistry) update(uuid, name, statusValue string, heightU int32) (Rack, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return Rack{}, fmt.Errorf("rack %s not found", uuid)
	}
	if name != "" {
		cur.Name = name
	}
	if statusValue != "" {
		cur.Status = statusValue
	}
	if heightU >= 0 {
		cur.HeightU = heightU
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return Rack{}, err
	}
	return cur, nil
}

// delete removes the row IF no hosts still reference it.
func (r *rackRegistry) delete(uuid string, hostCount int32) (int32, error) {
	if hostCount > 0 {
		return hostCount, fmt.Errorf("rack %s still has %d host(s) — drain first", uuid, hostCount)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return 0, fmt.Errorf("rack %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.byKey, rackKey{AZUUID: cur.AZUUID, Code: cur.Code})
	if err := r.saveLocked(); err != nil {
		r.byUUID[uuid] = cur
		r.byKey[rackKey{AZUUID: cur.AZUUID, Code: cur.Code}] = uuid
		return 0, err
	}
	return 0, nil
}
