package weft

// azs.go owns the AvailabilityZone registry — the top tier of the
// inventory hierarchy (AZ → Rack → Host). The proto's `code` field
// is the immutable short identifier scheduling rules + host
// registrations carry around to pin placement ; `name` / `region` /
// `status` are operator-mutable via UpdateAZ.
//
// Persistence follows the same Storage-interface pattern as the
// project registry : a single JSON document atomically replaced on
// every mutation. JSON (rather than HCL) keeps the registry small
// and stable for the eight scalar fields per row.
//
// Cascade safety : DeleteAZ refuses when racks or hosts still
// reference the AZ. The blocking counts surface back through the
// response so the operator sees exactly what needs draining.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// AZ is one entry in the registry.
type AZ struct {
	UUID      string    `json:"uuid"`
	Code      string    `json:"code"` // immutable short id, e.g. "DC-A"
	Name      string    `json:"name"`
	Region    string    `json:"region"`
	Status    string    `json:"status"` // "active" | "draining" | "down" | "provisioning"
	CreatedAt time.Time `json:"created_at"`
}

// azsDoc is the JSON top-level document.
type azsDoc struct {
	AZs []AZ `json:"azs"`
}

// azRegistryFileName is the conventional registry file under the
// state directory. Leading dot keeps it out of ListLocal's walk.
const azRegistryFileName = "azs"

// azRegistry is the in-memory cache backed by a Storage.
type azRegistry struct {
	mu       sync.Mutex
	storage  Storage
	byUUID   map[string]AZ
	codeIdx  map[string]string // code → uuid
}

// loadAZRegistry reads the blob via Storage. Empty blob → empty
// registry, same as projectRegistry.
func loadAZRegistry(ctx context.Context, storage Storage) (*azRegistry, error) {
	reg := &azRegistry{
		storage: storage,
		byUUID:  make(map[string]AZ),
		codeIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load az registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc azsDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse az registry: %w", err)
	}
	for _, a := range doc.AZs {
		reg.byUUID[a.UUID] = a
		reg.codeIdx[a.Code] = a.UUID
	}
	return reg, nil
}

// saveLocked serialises the cache to Storage. Caller holds mu.
// Output is sorted by UUID for stable diffs (the registry file is
// occasionally inspected by hand).
func (r *azRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := azsDoc{AZs: make([]AZ, 0, len(uuids))}
	for _, u := range uuids {
		doc.AZs = append(doc.AZs, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode az registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save az registry: %w", err)
	}
	return nil
}

// lookupByUUID returns the AZ for uuid + a found flag.
func (r *azRegistry) lookupByUUID(uuid string) (AZ, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byUUID[uuid]
	return a, ok
}

// lookupByCode resolves a code (e.g. "DC-A") to its full row.
func (r *azRegistry) lookupByCode(code string) (AZ, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.codeIdx[code]
	if !ok {
		return AZ{}, false
	}
	return r.byUUID[uuid], true
}

// list returns a sorted-by-Code copy of every AZ.
func (r *azRegistry) list() []AZ {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AZ, 0, len(r.byUUID))
	for _, a := range r.byUUID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// create registers a new AZ. Returns (row, true, nil) on insert,
// (existing, false, nil) when an AZ with the same code already
// lives in the registry — idempotent, mirrors getOrCreate on
// projects.
func (r *azRegistry) create(code, name, region, statusValue string) (AZ, bool, error) {
	if code == "" {
		return AZ{}, false, fmt.Errorf("az code must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.codeIdx[code]; ok {
		return r.byUUID[uuid], false, nil
	}
	if statusValue == "" {
		statusValue = "active"
	}
	uuid, err := mintAZUUID()
	if err != nil {
		return AZ{}, false, err
	}
	a := AZ{
		UUID:      uuid,
		Code:      code,
		Name:      name,
		Region:    region,
		Status:    statusValue,
		CreatedAt: time.Now().UTC(),
	}
	r.byUUID[uuid] = a
	r.codeIdx[code] = uuid
	if err := r.saveLocked(); err != nil {
		// Roll back the in-memory insert ; persistence is the
		// authoritative state.
		delete(r.byUUID, uuid)
		delete(r.codeIdx, code)
		return AZ{}, false, err
	}
	return a, true, nil
}

// update patches the mutable fields. Empty-string args = "keep
// current" (partial PATCH).
func (r *azRegistry) update(uuid, name, region, statusValue string) (AZ, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return AZ{}, fmt.Errorf("az %s not found", uuid)
	}
	if name != "" {
		cur.Name = name
	}
	if region != "" {
		cur.Region = region
	}
	if statusValue != "" {
		cur.Status = statusValue
	}
	r.byUUID[uuid] = cur
	if err := r.saveLocked(); err != nil {
		return AZ{}, err
	}
	return cur, nil
}

// delete removes the row IF no racks / hosts still reference it.
// rackCount + hostCount come from the caller (typically the
// Adapter, which has visibility on both other registries).
func (r *azRegistry) delete(uuid string, rackCount, hostCount int32) (int32, int32, error) {
	if rackCount > 0 || hostCount > 0 {
		return rackCount, hostCount, fmt.Errorf("az %s still has %d rack(s) and %d host(s) — drain first", uuid, rackCount, hostCount)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return 0, 0, fmt.Errorf("az %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.codeIdx, cur.Code)
	if err := r.saveLocked(); err != nil {
		// Re-insert on save failure so callers don't see a phantom
		// delete.
		r.byUUID[uuid] = cur
		r.codeIdx[cur.Code] = uuid
		return 0, 0, err
	}
	return 0, 0, nil
}

// mintAZUUID returns a fresh RFC 4122 v4 UUID using crypto/rand.
// Shared with the rack registry via mintRackUUID — both forms call
// the same helper so a future swap to a stronger ID scheme touches
// one place.
func mintAZUUID() (string, error) { return mintInventoryUUID() }

// mintInventoryUUID is the central UUID generator for the inventory
// registries (AZ + Rack).
func mintInventoryUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}
