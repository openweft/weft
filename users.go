package weft

// users.go owns the weft-internal user registry — the mapping
// (OIDC issuer + subject) ↔ stable weft UUID.
//
// Why this exists, given that every request already carries a
// fully-validated *Caller (see auth.go):
//
//   * OIDC subjects are stable per-IdP but not across IdP
//     migrations (LDAP→GitHub, dex→Auth0). A weft-local UUID
//     decouples persistent state (project ownership, VM
//     ownership, audit logs) from "which IdP issued the token".
//   * Operators may want to rename a user without rotating the
//     OIDC subject — the DisplayName field is the editable
//     surface; the UUID + Subject are immutable.
//   * Last-seen email + groups are recorded at every auth so the
//     operator-side tooling can show "who is X?" without
//     bouncing back to the upstream IdP.
//
// Wire model (mirrors projects.go):
//
//   * registry: <vmsDir>/.users.hcl   (single HCL document)
//   * one `user "<uuid>" { ... }` block per entry
//   * Storage interface (file / etcd / mem) handles persistence
//   * concurrent access serialised by an internal mutex
//
// Schema mirrors the project pattern for diff-friendly review:
//
//   user "abc-123-…" {
//     oidc_subject  = "00000000-0000-0000-0000-000000000001"
//     oidc_issuer   = "https://dex.example.com"
//     email         = "alice@example.com"
//     display_name  = "Alice Example"
//     groups        = ["team-alpha", "team-net"]
//     created_at    = "2026-05-23T08:21:14.123456Z"
//     last_seen_at  = "2026-05-23T09:42:01.987654Z"
//   }
//
// Phase D of [[etcd-control-plane]]: every new registry is
// Storage-backed from day one. Pick FileStorage in dev, EtcdStorage
// in prod, MemStorage in tests — `Adapter.storageFactory("users")`
// hides the choice from this file.

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

// User is one entry in the user registry. UUID and (Subject,
// Issuer) are immutable; the rest is refreshed on every successful
// auth via getOrCreateFromCaller.
type User struct {
	UUID        string    `json:"uuid"`
	Subject     string    `json:"oidc_subject"`
	Issuer      string    `json:"oidc_issuer"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Groups      []string  `json:"groups"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// usersDoc is the top-level HCL schema decoded from the registry
// file. Each `user "<uuid>" { … }` block carries one User.
type usersDoc struct {
	Users []userBlock `hcl:"user,block"`
}

// userBlock mirrors one HCL block. The label is the UUID; the
// body holds the operator-visible fields.
type userBlock struct {
	UUID        string   `hcl:",label"`
	Subject     string   `hcl:"oidc_subject"`
	Issuer      string   `hcl:"oidc_issuer"`
	Email       string   `hcl:"email,optional"`
	DisplayName string   `hcl:"display_name,optional"`
	Groups      []string `hcl:"groups,optional"`
	CreatedAt   string   `hcl:"created_at"`
	LastSeenAt  string   `hcl:"last_seen_at,optional"`
}

// userRegistry is the in-memory cache plus the helpers that keep
// it in sync with the Storage backend.
type userRegistry struct {
	mu        sync.Mutex
	storage   Storage
	byUUID    map[string]User
	subjectIdx map[string]string // "<issuer>\x00<subject>" → UUID
}

// loadUserRegistry reads the registry blob via Storage. Returns
// an empty registry when the Storage returns (nil, nil) — fresh
// install or first time the operator points weft at this backend.
func loadUserRegistry(ctx context.Context, storage Storage) (*userRegistry, error) {
	reg := &userRegistry{
		storage:    storage,
		byUUID:     make(map[string]User),
		subjectIdx: make(map[string]string),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load user registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc usersDoc
	if err := hclsimple.Decode("user-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse user registry: %w", err)
	}
	for _, b := range doc.Users {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		lastSeen, _ := time.Parse(time.RFC3339Nano, b.LastSeenAt)
		u := User{
			UUID:        b.UUID,
			Subject:     b.Subject,
			Issuer:      b.Issuer,
			Email:       b.Email,
			DisplayName: b.DisplayName,
			Groups:      append([]string(nil), b.Groups...),
			CreatedAt:   created,
			LastSeenAt:  lastSeen,
		}
		reg.byUUID[u.UUID] = u
		reg.subjectIdx[subjectIdxKey(u.Issuer, u.Subject)] = u.UUID
	}
	return reg, nil
}

// saveLocked writes the registry via Storage. Caller must hold mu.
// HCL output is sorted by UUID for stable diffs across weft runs.
func (r *userRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft user registry — UUID-keyed, see weft_uuid_keyed_resources.md\n" +
				"# Edit `display_name` to rename a user; never edit the block\n" +
				"# label (UUID) or `oidc_subject` / `oidc_issuer`.\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		usr := r.byUUID[u]
		block := body.AppendNewBlock("user", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("oidc_subject", cty.StringVal(usr.Subject))
		bb.SetAttributeValue("oidc_issuer", cty.StringVal(usr.Issuer))
		if usr.Email != "" {
			bb.SetAttributeValue("email", cty.StringVal(usr.Email))
		}
		if usr.DisplayName != "" {
			bb.SetAttributeValue("display_name", cty.StringVal(usr.DisplayName))
		}
		if len(usr.Groups) > 0 {
			vals := make([]cty.Value, len(usr.Groups))
			for i, g := range usr.Groups {
				vals[i] = cty.StringVal(g)
			}
			bb.SetAttributeValue("groups", cty.ListVal(vals))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(usr.CreatedAt.Format(time.RFC3339Nano)))
		if !usr.LastSeenAt.IsZero() {
			bb.SetAttributeValue("last_seen_at", cty.StringVal(usr.LastSeenAt.Format(time.RFC3339Nano)))
		}
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// subjectIdxKey is the composite key into subjectIdx. NUL is a
// safe separator — neither OIDC issuers (URLs) nor subjects
// (UUIDs / opaque strings) contain it.
func subjectIdxKey(issuer, subject string) string {
	return issuer + "\x00" + subject
}

// lookupByUUID returns (User, true) when the UUID is registered.
func (r *userRegistry) lookupByUUID(uuid string) (User, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byUUID[uuid]
	return u, ok
}

// lookupBySubject returns (User, true) when (issuer, subject) is
// registered. Empty issuer / subject is treated as "not present"
// even if the map happens to contain such an entry — dev-mode
// callers (anonymous, no real OIDC) never match.
func (r *userRegistry) lookupBySubject(issuer, subject string) (User, bool) {
	if issuer == "" || subject == "" {
		return User{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.subjectIdx[subjectIdxKey(issuer, subject)]
	if !ok {
		return User{}, false
	}
	u, ok := r.byUUID[uuid]
	return u, ok
}

// list returns every registered user, sorted by display name then
// UUID for deterministic test + tabular output.
func (r *userRegistry) list() []User {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]User, 0, len(r.byUUID))
	for _, u := range r.byUUID {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].UUID < out[j].UUID
	})
	return out
}

// getOrCreateFromCaller looks the user up by (issuer, subject) and
// either creates a fresh UUID entry, or refreshes Email / Groups /
// LastSeenAt on the existing record. The bool return indicates
// "was just created" so callers can audit-log first sights.
//
// Empty issuer (dev mode) is rejected — anonymous callers never
// hit the persistent registry. Phase B of multitenancy may add a
// "dev:<os-user>" synthetic registry, kept separate from the
// production OIDC-issued one.
func (r *userRegistry) getOrCreateFromCaller(c *Caller) (User, bool, error) {
	if c == nil || c.Subject == "" || c.Issuer == "" {
		return User{}, false, fmt.Errorf("getOrCreateFromCaller: anonymous / dev-mode Caller")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	key := subjectIdxKey(c.Issuer, c.Subject)
	if uuid, ok := r.subjectIdx[key]; ok {
		u := r.byUUID[uuid]
		// Refresh the volatile fields.
		u.Email = c.Email
		u.Groups = append([]string(nil), c.Groups...)
		u.LastSeenAt = now
		r.byUUID[uuid] = u
		if err := r.saveLocked(); err != nil {
			return User{}, false, err
		}
		return u, false, nil
	}
	u := User{
		UUID:        newUUID(),
		Subject:     c.Subject,
		Issuer:      c.Issuer,
		Email:       c.Email,
		DisplayName: c.Email, // sensible default; operator can override later
		Groups:      append([]string(nil), c.Groups...),
		CreatedAt:   now,
		LastSeenAt:  now,
	}
	r.byUUID[u.UUID] = u
	r.subjectIdx[key] = u.UUID
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, u.UUID)
		delete(r.subjectIdx, key)
		return User{}, false, err
	}
	return u, true, nil
}

// setDisplayName updates the operator-visible name on the user
// with the given UUID. The (Subject, Issuer, UUID) tuple stays
// frozen — that's the whole point of carrying a UUID.
func (r *userRegistry) setDisplayName(uuid, name string) error {
	if name == "" {
		return fmt.Errorf("empty display_name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("user %q not found", uuid)
	}
	u.DisplayName = name
	r.byUUID[uuid] = u
	return r.saveLocked()
}

// delete drops a user from the registry. Does NOT touch downstream
// state — callers must reassign any project ownership / VM
// ownership the deleted user held before invoking this.
func (r *userRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("user %q not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.subjectIdx, subjectIdxKey(u.Issuer, u.Subject))
	return r.saveLocked()
}
