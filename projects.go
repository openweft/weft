package weft

// projects.go owns the mapping (project display name ↔ project UUID)
// that lets weft rename a project without touching the on-disk path
// of any VM attached to it.
//
// Wire model:
//   * disk path: <vmsDir>/<uuid>/<vm-name>/
//   * registry: <vmsDir>/.projects.hcl    (single atomic file)
//
// The on-disk format is HCL — one `project "<uuid>"` block per
// entry, with `name` and `created_at` attributes. HCL is preferred
// over JSON for weft registries: comments are allowed and the shape
// stays readable when an operator pokes at it by hand.
//
// Resolution rules in ResolveProjectUUID:
//   1. empty input    → caller's default project (currently
//                       `usr-<weft-os-user>`; auto-created on first
//                       call so a fresh weft just works)
//   2. valid UUID     → return verbatim (callers can refer to
//                       projects by UUID for stability across
//                       renames)
//   3. display name   → look up in the registry; auto-create when
//                       missing so `--project=team-net` on first
//                       use just registers it. Phase 2 auth will
//                       gate this with create-permission.
//
// The registry is loaded once at startup, cached in memory, and
// rewritten (atomic temp + rename) on every mutation. Concurrent
// access is serialised by an internal mutex.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// Project is one entry in the registry.
//
// Members carries the user-UUIDs that have access to the project
// independently of any dex group claim. Two ways to grant access
// today (per [[weft-uuid-keyed-resources]]'s ACL model):
//
//   1. dex issues the caller a `project:<uuid>` group → caller's
//      token already carries the grant.
//   2. an admin runs `weft project add-user <project-uuid>
//      <user-uuid>` → the user's UUID lands in Members and
//      callerOwnsProject resolves it via the user registry.
//
// Both paths union into VisibleProjects; either is sufficient.
type Project struct {
	UUID      string    `json:"uuid"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Members   []string  `json:"members,omitempty"`

	// NATSUserSeed is the project's NATS user NKey seed ("SU…"),
	// minted lazily on first RegisterMicroVM for the project and
	// materialised into each microVM under <vmDir>/nats/nats.nkey.
	// Per [[weft-tenant-event-access]] Phase 2: enables tenant
	// workloads to authenticate to the bus from inside the guest
	// (Phase 3 wires the matching server-side subject permissions).
	// Sensitive material — kept off `weft project show` output by
	// default; only admin-scoped diagnostics surface the public key.
	NATSUserSeed string `json:"nats_user_seed,omitempty"`
}

// registryFileName is the relative path inside vmsDir where the
// project metadata lives. The leading dot keeps it out of
// ListLocal's normal walk (which is keyed on os.ReadDir entries).
const registryFileName = ".projects.hcl"

// projectsDoc is the top-level HCL schema decoded from the
// registry file. Each `project "<uuid>" { … }` block carries the
// display name + creation timestamp.
type projectsDoc struct {
	Projects []projectBlock `hcl:"project,block"`
}

// projectBlock mirrors one HCL block. The block label is the UUID;
// the body carries the operator-visible fields.
type projectBlock struct {
	UUID         string   `hcl:",label"`
	Name         string   `hcl:"name"`
	CreatedAt    string   `hcl:"created_at"`
	Members      []string `hcl:"members,optional"`
	NATSUserSeed string   `hcl:"nats_user_seed,optional"`
}

// projectRegistry is the in-memory cache of the on-disk registry
// plus the helpers that keep it in sync with the Storage backend.
//
// Persistence is delegated to Storage (see storage.go): file
// backend for single-host dev, etcd backend for the 3-DC control
// plane (etcd_control_plane.md), or in-memory for tests. The
// registry code doesn't care which one is wired underneath.
type projectRegistry struct {
	mu      sync.Mutex
	storage Storage
	byUUID  map[string]Project
	nameIdx map[string]string // display name → uuid (case-sensitive)
}

// loadProjectRegistry reads the registry blob via Storage. A
// Storage that returns (nil, nil) — fresh install — yields an
// empty registry; the adapter populates it on demand.
func loadProjectRegistry(ctx context.Context, storage Storage) (*projectRegistry, error) {
	reg := &projectRegistry{
		storage: storage,
		byUUID:  make(map[string]Project),
		nameIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load project registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc projectsDoc
	if err := hclsimple.Decode("project-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse project registry: %w", err)
	}
	for _, b := range doc.Projects {
		// Self-heal: the block label is authoritative for the UUID.
		ts, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		p := Project{
			UUID:         b.UUID,
			Name:         b.Name,
			CreatedAt:    ts,
			Members:      append([]string(nil), b.Members...),
			NATSUserSeed: b.NATSUserSeed,
		}
		reg.byUUID[p.UUID] = p
		reg.nameIdx[p.Name] = p.UUID
	}
	return reg, nil
}

// saveLocked writes the registry via Storage. Caller must hold mu.
// The HCL output is sorted by UUID for stable diffs.
func (r *projectRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  0,
		Bytes: []byte("# weft project registry — UUID-keyed, see weft_uuid_keyed_resources.md\n# Edit `name` to rename a project; never edit the block label (UUID).\n\n"),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	// Stable, deterministic ordering for diff-friendly output.
	for i := 1; i < len(uuids); i++ {
		for j := i; j > 0 && uuids[j-1] > uuids[j]; j-- {
			uuids[j-1], uuids[j] = uuids[j], uuids[j-1]
		}
	}
	for _, u := range uuids {
		p := r.byUUID[u]
		block := body.AppendNewBlock("project", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("name", cty.StringVal(p.Name))
		bb.SetAttributeValue("created_at", cty.StringVal(p.CreatedAt.Format(time.RFC3339Nano)))
		if p.NATSUserSeed != "" {
			bb.SetAttributeValue("nats_user_seed", cty.StringVal(p.NATSUserSeed))
		}
		if len(p.Members) > 0 {
			// Stable sort for diff-friendly output.
			members := append([]string(nil), p.Members...)
			for i := 1; i < len(members); i++ {
				for j := i; j > 0 && members[j-1] > members[j]; j-- {
					members[j-1], members[j] = members[j], members[j-1]
				}
			}
			vals := make([]cty.Value, len(members))
			for i, m := range members {
				vals[i] = cty.StringVal(m)
			}
			bb.SetAttributeValue("members", cty.ListVal(vals))
		}
		body.AppendNewline()
	}
	// Storage is responsible for atomic-replace semantics
	// (FileStorage does tmp+rename; etcd Put is naturally
	// linearizable; MemStorage just overwrites). The caller of
	// saveLocked already holds the registry mutex, so this Save
	// is serialized w.r.t. other in-process mutators.
	return r.storage.Save(context.Background(), f.Bytes())
}

// lookupByName returns (uuid, true) when a project with the given
// display name exists.
func (r *projectRegistry) lookupByName(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[name]
	return uuid, ok
}

// lookupByUUID returns (Project, true) when the UUID is registered.
func (r *projectRegistry) lookupByUUID(uuid string) (Project, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	return p, ok
}

// list returns a copy of every registered project, sorted by name
// (deterministic for tests + tabular output).
func (r *projectRegistry) list() []Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Project, 0, len(r.byUUID))
	for _, p := range r.byUUID {
		out = append(out, p)
	}
	// Sort by display name; stable enough since names are unique.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Name > out[j].Name; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// getOrCreate looks the project up by name and creates a fresh
// UUID entry when missing. Returns the canonical Project.
func (r *projectRegistry) getOrCreate(name string) (Project, bool, error) {
	if name == "" {
		return Project{}, false, fmt.Errorf("empty project name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if uuid, ok := r.nameIdx[name]; ok {
		return r.byUUID[uuid], false, nil
	}
	p := Project{
		UUID:      newUUID(),
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}
	r.byUUID[p.UUID] = p
	r.nameIdx[p.Name] = p.UUID
	if err := r.saveLocked(); err != nil {
		// Roll back to keep the in-memory state consistent with
		// what's on disk.
		delete(r.byUUID, p.UUID)
		delete(r.nameIdx, p.Name)
		return Project{}, false, err
	}
	return p, true, nil
}

// rename updates the display name of the project with the given
// UUID. The UUID (and therefore every VM path) stays unchanged —
// that's the whole point of carrying a UUID.
func (r *projectRegistry) rename(uuid, newName string) error {
	if newName == "" {
		return fmt.Errorf("empty new name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("project %q not found", uuid)
	}
	if existing, taken := r.nameIdx[newName]; taken && existing != uuid {
		return fmt.Errorf("name %q already in use by project %s", newName, existing)
	}
	delete(r.nameIdx, p.Name)
	p.Name = newName
	r.byUUID[uuid] = p
	r.nameIdx[newName] = uuid
	if err := r.saveLocked(); err != nil {
		return err
	}
	return nil
}

// addMember appends `userUUID` to the project's Members list.
// Idempotent: a user already in the list is a no-op. Refuses an
// unknown project UUID. The membership doubles the dex `groups`
// claim path — both are checked in callerOwnsProject + folded
// into VisibleProjects.
func (r *projectRegistry) addMember(projectUUID, userUUID string) error {
	if userUUID == "" {
		return fmt.Errorf("empty user_uuid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[projectUUID]
	if !ok {
		return fmt.Errorf("project %q not found", projectUUID)
	}
	for _, m := range p.Members {
		if m == userUUID {
			return nil
		}
	}
	p.Members = append(p.Members, userUUID)
	r.byUUID[projectUUID] = p
	return r.saveLocked()
}

// removeMember drops `userUUID` from the project's Members list.
// Idempotent: a user not in the list is a no-op (no error).
func (r *projectRegistry) removeMember(projectUUID, userUUID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[projectUUID]
	if !ok {
		return fmt.Errorf("project %q not found", projectUUID)
	}
	out := p.Members[:0:0]
	changed := false
	for _, m := range p.Members {
		if m == userUUID {
			changed = true
			continue
		}
		out = append(out, m)
	}
	if !changed {
		return nil
	}
	p.Members = out
	r.byUUID[projectUUID] = p
	return r.saveLocked()
}

// ensureNATSUserSeed returns the project's NATS user-NKey seed,
// minting it on first call and persisting through Storage. The
// helper is idempotent: a project that already carries a seed
// gets the cached value back with no Storage write. Used by
// RegisterMicroVM to materialise <vmDir>/nats/nats.nkey, and by
// future server-config rendering to derive the matching pubkey.
//
// Returns an error iff key generation or persistence fails — the
// caller treats this as fatal for the VM register, since a
// half-provisioned project (members but no seed) is incoherent
// for Phase-2 onwards.
func (r *projectRegistry) ensureNATSUserSeed(projectUUID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[projectUUID]
	if !ok {
		return "", fmt.Errorf("project %q not found", projectUUID)
	}
	if p.NATSUserSeed != "" {
		return p.NATSUserSeed, nil
	}
	nk, err := generateProjectUserNKey()
	if err != nil {
		return "", err
	}
	p.NATSUserSeed = nk.Seed
	r.byUUID[projectUUID] = p
	if err := r.saveLocked(); err != nil {
		// Roll back so the in-memory state matches what's on disk.
		p.NATSUserSeed = ""
		r.byUUID[projectUUID] = p
		return "", err
	}
	return nk.Seed, nil
}

// members returns a defensive copy of the project's member list.
func (r *projectRegistry) members(projectUUID string) ([]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[projectUUID]
	if !ok {
		return nil, false
	}
	return append([]string(nil), p.Members...), true
}

// delete drops a project from the registry. Does NOT touch the
// on-disk vmDirs; callers ensure the project subdir is empty first.
func (r *projectRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("project %q not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, p.Name)
	if err := r.saveLocked(); err != nil {
		return err
	}
	return nil
}

// uuidPattern matches a canonical 8-4-4-4-12 hex UUID. Used by
// the resolver to decide whether a caller-supplied string is a
// UUID (skip name lookup) or a display name.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// newUUID returns a fresh random 8-4-4-4-12 hex UUID. We avoid the
// google/uuid dep to keep weft's import surface minimal; the bytes
// come from crypto/rand, which is plenty for unique identifiers.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the host RNG is broken — fall
		// back to a timestamp-based ID so weft at least keeps running.
		ns := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(ns >> (i * 8))
		}
	}
	// RFC 4122 v4 bits — not strictly required since we just need
	// uniqueness, but keeps the value parseable by every uuid
	// library that may see it.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return strings.Join([]string{h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]}, "-")
}

// defaultProjectName is the display name used for the auto-created
// project for the OS user weft runs as. Phase 2 swaps this for the
// authenticated caller's identity.
func defaultProjectName() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return "usr-unknown"
	}
	return "usr-" + u.Username
}
