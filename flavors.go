package weft

// flavors.go — compute-envelope catalogue (cluster-wide), persisted
// via Storage (FileStorage / MemStorage / EtcdStorage — see
// storage.go). Operators name a sizing bundle ("small", "gpu-large")
// + the catalogue serves these to weft-webui's CreateVMModal and to
// weft-cli's `weft flavor` subcommand (queued).
//
// Wire model — same shape as projects.go :
//   * disk path : <vmsDir>/.flavors.hcl  (file backend, single host)
//   * etcd key  : <prefix>flavors        (HA, 3-DC ; same blob)
//   * one `flavor "<name>" { … }` HCL block per entry
//
// Keyed by NAME (operator-visible) rather than UUID : flavors are
// referenced by name in CreateVMRequest, the catalogue is editable
// without surprise renames, and the blob stays human-readable for
// operators editing by hand. Names are unique cluster-wide — a
// `large` in tenant A is the SAME envelope as `large` in tenant B
// (catalogues aren't tenant-scoped in this iteration ; the proto
// extension that would tenant-scope them lands separately).
//
// Persistence pattern : loaded once at startup, cached in memory,
// re-saved atomically on every mutation. Concurrent access is
// serialised by an internal mutex ; Storage's Save provides the
// atomic-replace semantics (tmp+rename for file, Put for etcd).
//
// HCL was picked over JSON for the same reason projects.go did :
// comments allowed, the shape stays readable when an operator pokes
// at it by hand.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// Flavor is the operator-visible entry in the catalogue. RAM is
// kept as a string ("4Gi" / "256Mi" / raw number = MB) so the on-
// disk file stays operator-editable ; weft-webui's
// internal/server/lifecycle.go parses to MB at the wire boundary.
//
// GPU is empty when no GPU is required ; a non-empty value
// ("1×A100-40G") pins matching VMs to hosts that physically carry
// the model.
type Flavor struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	VCPU        int    `json:"vcpu"`
	RAM         string `json:"ram"`
	EphemeralGB int    `json:"ephemeral_gb"`
	GPU         string `json:"gpu,omitempty"`
}

// flavorsRegistryFileName is the conventional file path relative to
// vmsDir, used by the file backend. The leading dot keeps it out of
// ListLocal's normal walk (same convention as .projects.hcl).
const flavorsRegistryFileName = ".flavors.hcl"

// flavorsDoc / flavorBlock mirror the HCL schema. The block label is
// the flavor's name ; the body carries the envelope.
type flavorsDoc struct {
	Flavors []flavorBlock `hcl:"flavor,block"`
}
type flavorBlock struct {
	Name        string `hcl:",label"`
	UUID        string `hcl:"uuid,optional"` // V0.13.1 — backfilled from name when missing on load
	VCPU        int    `hcl:"vcpu"`
	RAM         string `hcl:"ram"`
	EphemeralGB int    `hcl:"ephemeral_gb"`
	GPU         string `hcl:"gpu,optional"`
}

// FlavorRegistry is the in-memory cache + Storage-backed persistence
// layer. Same shape as projectRegistry ; the caller constructs one
// at startup via LoadFlavorRegistry(ctx, storage) and uses it through
// the public methods below (List / Get / Set / Delete).
//
// Exported (capital F) because cmd/weft needs to construct one
// directly and hold it on the gRPC server struct — flavors don't go
// through the ACL-bearing adapter the way projects do, so the
// registry is a direct dependency rather than an adapter method.
type FlavorRegistry = flavorRegistry

type flavorRegistry struct {
	mu      sync.Mutex
	storage Storage
	byName  map[string]Flavor
}

// LoadFlavorRegistry reads the blob via Storage. Exported facade
// over loadFlavorRegistry for cmd/weft.
func LoadFlavorRegistry(ctx context.Context, storage Storage) (*FlavorRegistry, error) {
	return loadFlavorRegistry(ctx, storage)
}

// loadFlavorRegistry reads the blob via Storage. A Storage returning
// (nil, nil) — fresh install — yields an empty registry. The caller
// can then seed it via Set ; operators bootstrapping a new cluster
// typically run `weft flavor create --from-defaults` (queued).
func loadFlavorRegistry(ctx context.Context, storage Storage) (*flavorRegistry, error) {
	reg := &flavorRegistry{
		storage: storage,
		byName:  make(map[string]Flavor),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load flavor registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc flavorsDoc
	if err := hclsimple.Decode("flavor-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse flavor registry: %w", err)
	}
	for _, b := range doc.Flavors {
		uuid := b.UUID
		if uuid == "" {
			// V0.13.1 lazy backfill : deterministic UUIDv5 from the
			// name so the migration is idempotent across agents +
			// stable across restarts. Older blocks (no `uuid =`
			// line) get a UUID the first time the registry rewrites
			// itself.
			uuid = flavorUUIDFromName(b.Name)
		}
		reg.byName[b.Name] = Flavor{
			UUID: uuid,
			Name: b.Name, VCPU: b.VCPU, RAM: b.RAM,
			EphemeralGB: b.EphemeralGB, GPU: b.GPU,
		}
	}
	return reg, nil
}

// flavorUUIDFromName derives a stable UUIDv5-style identifier from
// the flavor's name. Used for the V0.13.1 lazy migration of legacy
// flavor blocks : every load that finds a missing `uuid` field
// fills it via this helper, idempotent across agents because the
// hash is deterministic. New flavors created via Set get a fresh
// random v4 UUID (different code path, see Set).
func flavorUUIDFromName(name string) string {
	sum := sha256.Sum256([]byte("openweft/flavor/" + name))
	h := hex.EncodeToString(sum[:16])
	// Pin variant + version bits the same way mintInventoryUUID
	// does so the output is visually indistinguishable from a
	// random v4 UUID.
	b, _ := hex.DecodeString(h)
	b[6] = (b[6] & 0x0f) | 0x50 // version 5 (name-based SHA-1 family)
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h = hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// saveLocked writes the registry via Storage. Caller must hold mu.
// Output is sorted by name for diff-friendly HCL.
func (r *flavorRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  0,
		Bytes: []byte("# weft flavor catalogue — cluster-wide compute envelopes.\n# Names are the stable identifier ; cpu/ram/disk are operator-set.\n# Edit + reload via `weft flavor` ; the dashboard reads this blob too.\n\n"),
	}})
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fl := r.byName[n]
		block := body.AppendNewBlock("flavor", []string{n})
		bb := block.Body()
		if fl.UUID != "" {
			bb.SetAttributeValue("uuid", cty.StringVal(fl.UUID))
		}
		bb.SetAttributeValue("vcpu", cty.NumberIntVal(int64(fl.VCPU)))
		bb.SetAttributeValue("ram", cty.StringVal(fl.RAM))
		bb.SetAttributeValue("ephemeral_gb", cty.NumberIntVal(int64(fl.EphemeralGB)))
		if fl.GPU != "" {
			bb.SetAttributeValue("gpu", cty.StringVal(fl.GPU))
		}
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// List returns a copy of every flavor, sorted by name for stable
// callers. Cheap : the catalogue is bounded by operator inputs (a
// handful of CPU tiers + GPU SKUs ; not user data).
func (r *flavorRegistry) List() []Flavor {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.byName))
	for n := range r.byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Flavor, 0, len(names))
	for _, n := range names {
		out = append(out, r.byName[n])
	}
	return out
}

// Get returns the named flavor, or false when missing.
func (r *flavorRegistry) Get(name string) (Flavor, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.byName[name]
	return f, ok
}

// Set upserts a flavor. Validation lives here (not at the RPC layer)
// so the file backend also catches an operator hand-edit that landed
// nonsense — Load + Set form the canonical sanity path.
func (r *flavorRegistry) Set(f Flavor) error {
	if f.Name == "" {
		return errors.New("flavor name is required")
	}
	if f.VCPU <= 0 {
		return errors.New("vcpu must be > 0")
	}
	if f.RAM == "" {
		return errors.New("ram is required (e.g. \"4Gi\")")
	}
	if f.EphemeralGB < 0 {
		return errors.New("ephemeral_gb must be >= 0")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Preserve a stable UUID across re-Set : if the caller passed
	// an empty UUID, reuse the one already in the registry (or
	// derive deterministically from the name on first insert).
	// Keeps the wire-stable handle stable through edits.
	if f.UUID == "" {
		if existing, ok := r.byName[f.Name]; ok && existing.UUID != "" {
			f.UUID = existing.UUID
		} else {
			f.UUID = flavorUUIDFromName(f.Name)
		}
	}
	r.byName[f.Name] = f
	return r.saveLocked()
}

// Delete removes a flavor. Idempotent : a missing name returns nil
// (matching the SPA's "deleted" semantic that doesn't surface a
// confusing 404 on retry).
func (r *flavorRegistry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; !ok {
		return nil
	}
	delete(r.byName, name)
	return r.saveLocked()
}
