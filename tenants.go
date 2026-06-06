package weft

// tenants.go owns the Tenant registry — top-level multi-tenant
// boundary above the Project registry. A tenant is a collection of
// projects + admins + members ; mirrors the AZ + project registry
// patterns (Storage-backed JSON document, atomic replace on every
// mutation, single mutex for concurrent access).
//
// Wire model (mirrors proto v0.6+ TenantInfo) :
//   - uuid              server-minted RFC4122 v4
//   - name              operator-facing display name (unique)
//   - domain            optional DNS-style domain (e.g. acme.example.com)
//   - status            "active" | "disabled"
//   - created_at        time.Time, serialised UnixNano on the wire
//   - admins []email    tenant-admin role grant list
//   - members []entry   email → groups[] (project pinning at member-add
//                       time, exposed as a single repeated `--group`
//                       flag on the CLI)
//
// Counts (projects / admins / members) on TenantInfo are server-side
// derived ; the on-disk document stores only the source-of-truth
// admin / member lists. ListTenants strips the member detail (only
// counts surface) ; per-tenant inspection RPCs are intentionally
// not exposed yet — the proto only carries Add/Remove verbs.
//
// Persistence : JSON via Storage interface, same shape as azs.go.
// File-backed under <vmsDir>/tenants in dev ; etcd-backed in HA.
//
// Cascade : DeleteTenant is currently unconditional because the
// Project struct does not carry a TenantUUID field (no project ↔
// tenant linkage exists in the codebase today). When projects gain
// a TenantUUID column, refuse-on-cascade lands here mirroring
// azRegistry.delete's blocking-count contract.

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

// TenantMember pairs an email with the project-group set granted at
// member-add time. Groups are project display names or UUIDs ; the
// CLI exposes them as repeatable `--group` flags.
type TenantMember struct {
	Email  string   `json:"email"`
	Groups []string `json:"groups,omitempty"`
}

// Tenant is one entry in the registry.
type Tenant struct {
	UUID      string         `json:"uuid"`
	Name      string         `json:"name"`
	Domain    string         `json:"domain,omitempty"`
	Status    string         `json:"status"` // "active" | "disabled"
	CreatedAt time.Time      `json:"created_at"`
	Admins    []string       `json:"admins,omitempty"`  // emails
	Members   []TenantMember `json:"members,omitempty"` // email → groups
}

// tenantsDoc is the JSON top-level document.
type tenantsDoc struct {
	Tenants []Tenant `json:"tenants"`
}

// tenantRegistryFileName is the conventional registry file under the
// state directory ; matches the azs.go convention (no leading dot,
// the Storage backends decide their own naming).
const tenantRegistryFileName = "tenants"

// tenantRegistry is the in-memory cache backed by a Storage.
type tenantRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]Tenant
	nameIdx map[string]string // name → uuid (case-sensitive)
}

// loadTenantRegistry reads the blob via Storage. Empty blob → empty
// registry, same as azRegistry / projectRegistry.
func loadTenantRegistry(ctx context.Context, storage Storage) (*tenantRegistry, error) {
	reg := &tenantRegistry{
		storage: storage,
		byUUID:  make(map[string]Tenant),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load tenant registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc tenantsDoc
	if err := json.Unmarshal(blob, &doc); err != nil {
		return nil, fmt.Errorf("parse tenant registry: %w", err)
	}
	for _, t := range doc.Tenants {
		reg.byUUID[t.UUID] = t
		reg.nameIdx[t.Name] = t.UUID
	}
	return reg, nil
}

// saveLocked serialises the cache to Storage. Caller holds mu.
// Output is sorted by UUID for stable diffs.
func (r *tenantRegistry) saveLocked() error {
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	doc := tenantsDoc{Tenants: make([]Tenant, 0, len(uuids))}
	for _, u := range uuids {
		doc.Tenants = append(doc.Tenants, r.byUUID[u])
	}
	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode tenant registry: %w", err)
	}
	if err := r.storage.Save(context.Background(), blob); err != nil {
		return fmt.Errorf("save tenant registry: %w", err)
	}
	return nil
}

// lookupByUUID returns the Tenant + found flag.
func (r *tenantRegistry) lookupByUUID(uuid string) (Tenant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byUUID[uuid]
	return t, ok
}

// lookupByName resolves a display name to its full row.
func (r *tenantRegistry) lookupByName(name string) (Tenant, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[name]
	if !ok {
		return Tenant{}, false
	}
	return r.byUUID[uuid], true
}

// list returns a sorted-by-Name copy of every tenant.
func (r *tenantRegistry) list() []Tenant {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Tenant, 0, len(r.byUUID))
	for _, t := range r.byUUID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// create registers a new tenant. Returns (row, true, nil) on insert,
// (existing, false, nil) when the name already exists — idempotent,
// same shape as azRegistry.create.
func (r *tenantRegistry) create(name, domain string) (Tenant, bool, error) {
	if name == "" {
		return Tenant{}, false, fmt.Errorf("tenant name must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[name]; ok {
		return r.byUUID[uuid], false, nil
	}
	uuid, err := mintTenantUUID()
	if err != nil {
		return Tenant{}, false, err
	}
	t := Tenant{
		UUID:      uuid,
		Name:      name,
		Domain:    domain,
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	r.byUUID[uuid] = t
	r.nameIdx[name] = uuid
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, uuid)
		delete(r.nameIdx, name)
		return Tenant{}, false, err
	}
	return t, true, nil
}

// delete drops the row. blockedProjects is surfaced so DeleteTenant
// can return the same blocking-count contract as DeleteAZ when a
// future schema adds a Project.TenantUUID field. Today projects
// don't reference tenants ; callers pass 0.
func (r *tenantRegistry) delete(uuid string, blockedProjects int32) (int32, error) {
	if blockedProjects > 0 {
		return blockedProjects, fmt.Errorf("tenant %s still has %d project(s) attached — delete them first", uuid, blockedProjects)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byUUID[uuid]
	if !ok {
		return 0, fmt.Errorf("tenant %s not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, cur.Name)
	if err := r.saveLocked(); err != nil {
		// Re-insert on save failure.
		r.byUUID[uuid] = cur
		r.nameIdx[cur.Name] = uuid
		return 0, err
	}
	return 0, nil
}

// addAdmin appends an admin email. Idempotent.
func (r *tenantRegistry) addAdmin(tenantUUID, email string) (Tenant, error) {
	if email == "" {
		return Tenant{}, fmt.Errorf("empty email")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byUUID[tenantUUID]
	if !ok {
		return Tenant{}, fmt.Errorf("tenant %s not found", tenantUUID)
	}
	for _, a := range t.Admins {
		if a == email {
			return t, nil
		}
	}
	t.Admins = append(t.Admins, email)
	r.byUUID[tenantUUID] = t
	if err := r.saveLocked(); err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// removeAdmin drops an admin email. Idempotent (missing email = no-op,
// no error).
func (r *tenantRegistry) removeAdmin(tenantUUID, email string) (Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byUUID[tenantUUID]
	if !ok {
		return Tenant{}, fmt.Errorf("tenant %s not found", tenantUUID)
	}
	out := t.Admins[:0:0]
	changed := false
	for _, a := range t.Admins {
		if a == email {
			changed = true
			continue
		}
		out = append(out, a)
	}
	if !changed {
		return t, nil
	}
	t.Admins = out
	r.byUUID[tenantUUID] = t
	if err := r.saveLocked(); err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// addMember inserts or updates an (email, groups) member entry. If
// the email already exists, the supplied groups are merged (union)
// into the existing set ; otherwise a new entry lands.
func (r *tenantRegistry) addMember(tenantUUID, email string, groups []string) (Tenant, error) {
	if email == "" {
		return Tenant{}, fmt.Errorf("empty email")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byUUID[tenantUUID]
	if !ok {
		return Tenant{}, fmt.Errorf("tenant %s not found", tenantUUID)
	}
	found := false
	for i, m := range t.Members {
		if m.Email == email {
			t.Members[i].Groups = unionStrings(m.Groups, groups)
			found = true
			break
		}
	}
	if !found {
		t.Members = append(t.Members, TenantMember{
			Email:  email,
			Groups: append([]string(nil), groups...),
		})
	}
	r.byUUID[tenantUUID] = t
	if err := r.saveLocked(); err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// removeMember drops a member entry by email. Idempotent.
func (r *tenantRegistry) removeMember(tenantUUID, email string) (Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.byUUID[tenantUUID]
	if !ok {
		return Tenant{}, fmt.Errorf("tenant %s not found", tenantUUID)
	}
	out := t.Members[:0:0]
	changed := false
	for _, m := range t.Members {
		if m.Email == email {
			changed = true
			continue
		}
		out = append(out, m)
	}
	if !changed {
		return t, nil
	}
	t.Members = out
	r.byUUID[tenantUUID] = t
	if err := r.saveLocked(); err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// unionStrings merges two slices preserving the first slice's order
// and appending newcomers from the second in input order. Deduped.
func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range b {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// mintTenantUUID returns a fresh RFC4122 v4 UUID. Distinct from
// mintInventoryUUID for grep-ability ; same crypto/rand backend.
func mintTenantUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]), nil
}
