package weft

// volumes.go owns the per-project block-volume registry: each
// entry tracks a piece of guest-attached storage the platform
// provisions (data disks, scratch volumes, future cinder-style
// portable volumes).
//
// Pattern is the multi-tenant one introduced by networks.go:
// volume names are scoped per project, so two projects can both
// own a "data" volume. The single global registry blob carries
// every project's entries; (projectUUID, name) indices keep
// lookups O(1).
//
// Schema:
//
//   volume "abc-…" {
//     project_uuid = "p-…"
//     name         = "data"
//     size_gib     = 100
//     format       = "raw"           # raw | qcow2
//     attached_to  = "vm-uuid-…"     # optional, empty = detached
//     created_at   = "..."
//   }
//
// Lifecycle rules:
//
//   * Size is immutable on shrink (resize() refuses smaller). Grow
//     is allowed and merely updates the registry; the actual file-
//     extend is the caller's problem (vzd's storage backend is the
//     ground truth for the byte layout).
//   * delete() refuses when AttachedTo != "" — explicit detach
//     first. Prevents orphaning a guest's data disk.
//   * (UUID, ProjectUUID, CreatedAt) are immutable.

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

// VolumeFormat enumerates the on-disk layouts vzd writes.
type VolumeFormat string

const (
	VolumeFormatRaw   VolumeFormat = "raw"   // default — flat, sparse-friendly
	VolumeFormatQCOW2 VolumeFormat = "qcow2" // copy-on-write snapshots
)

// Volume is one entry in the volume registry. UUID, ProjectUUID,
// Format, and CreatedAt are immutable; Name, SizeGiB, and
// AttachedTo are mutable via the dedicated setters.
type Volume struct {
	UUID        string       `json:"uuid"`
	ProjectUUID string       `json:"project_uuid"`
	Name        string       `json:"name"`
	SizeGiB     int          `json:"size_gib"`
	Format      VolumeFormat `json:"format"`
	AttachedTo  string       `json:"attached_to,omitempty"` // empty = detached
	CreatedAt   time.Time    `json:"created_at"`
}

// volumesDoc is the top-level HCL schema for the registry blob.
type volumesDoc struct {
	Volumes []volumeBlock `hcl:"volume,block"`
}

// volumeBlock mirrors one HCL block. The label is the UUID.
type volumeBlock struct {
	UUID        string `hcl:",label"`
	ProjectUUID string `hcl:"project_uuid"`
	Name        string `hcl:"name"`
	SizeGiB     int    `hcl:"size_gib"`
	Format      string `hcl:"format,optional"`
	AttachedTo  string `hcl:"attached_to,optional"`
	CreatedAt   string `hcl:"created_at"`
}

// volumeRegistry mirrors networkRegistry's shape — same indexes
// for the same reasons (multi-tenant name scoping + O(1)
// per-project listing).
type volumeRegistry struct {
	mu         sync.Mutex
	storage    Storage
	byUUID     map[string]Volume
	nameIdx    map[string]string                // (projectUUID,name) → UUID
	projectIdx map[string]map[string]struct{}   // projectUUID → set-of-UUIDs
}

// loadVolumeRegistry reads the blob via Storage. Empty / absent
// blob yields an empty registry.
func loadVolumeRegistry(ctx context.Context, storage Storage) (*volumeRegistry, error) {
	reg := &volumeRegistry{
		storage:    storage,
		byUUID:     make(map[string]Volume),
		nameIdx:    make(map[string]string),
		projectIdx: make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load volume registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc volumesDoc
	if err := hclsimple.Decode("volume-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse volume registry: %w", err)
	}
	for _, b := range doc.Volumes {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		format := VolumeFormat(b.Format)
		if format == "" {
			format = VolumeFormatRaw
		}
		v := Volume{
			UUID:        b.UUID,
			ProjectUUID: b.ProjectUUID,
			Name:        b.Name,
			SizeGiB:     b.SizeGiB,
			Format:      format,
			AttachedTo:  b.AttachedTo,
			CreatedAt:   created,
		}
		reg.byUUID[v.UUID] = v
		reg.nameIdx[volumeNameKey(v.ProjectUUID, v.Name)] = v.UUID
		if _, ok := reg.projectIdx[v.ProjectUUID]; !ok {
			reg.projectIdx[v.ProjectUUID] = make(map[string]struct{})
		}
		reg.projectIdx[v.ProjectUUID][v.UUID] = struct{}{}
	}
	return reg, nil
}

// volumeNameKey composes the secondary index key. NUL is safe —
// neither UUIDs nor volume names ever contain it.
func volumeNameKey(projectUUID, name string) string {
	return projectUUID + "\x00" + name
}

// saveLocked writes via Storage. Caller must hold mu. Output is
// sorted by UUID for stable diffs across vzd runs.
func (r *volumeRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# vzd volume registry — UUID-keyed per [[vzd-uuid-keyed-resources]].\n" +
				"# Edit `name` freely; never change the block label (UUID),\n" +
				"# `project_uuid`, or `format`. `size_gib` is grow-only.\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		v := r.byUUID[u]
		block := body.AppendNewBlock("volume", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("project_uuid", cty.StringVal(v.ProjectUUID))
		bb.SetAttributeValue("name", cty.StringVal(v.Name))
		bb.SetAttributeValue("size_gib", cty.NumberIntVal(int64(v.SizeGiB)))
		if v.Format != "" {
			bb.SetAttributeValue("format", cty.StringVal(string(v.Format)))
		}
		if v.AttachedTo != "" {
			bb.SetAttributeValue("attached_to", cty.StringVal(v.AttachedTo))
		}
		bb.SetAttributeValue("created_at", cty.StringVal(v.CreatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// validateVolumeFormat refuses unknown VolumeFormat values. Empty
// is allowed (defaults to raw at create time).
func validateVolumeFormat(f VolumeFormat) error {
	switch f {
	case "", VolumeFormatRaw, VolumeFormatQCOW2:
		return nil
	default:
		return fmt.Errorf("unknown volume format %q (want raw or qcow2)", f)
	}
}

// lookupByUUID returns (Volume, true) when the UUID is known.
func (r *volumeRegistry) lookupByUUID(uuid string) (Volume, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	return v, ok
}

// lookupByName resolves (projectUUID, name) → Volume.
func (r *volumeRegistry) lookupByName(projectUUID, name string) (Volume, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuid, ok := r.nameIdx[volumeNameKey(projectUUID, name)]
	if !ok {
		return Volume{}, false
	}
	v, ok := r.byUUID[uuid]
	return v, ok
}

// listForProject returns every volume owned by the project,
// sorted by name.
func (r *volumeRegistry) listForProject(projectUUID string) []Volume {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.projectIdx[projectUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]Volume, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// list returns every registered volume across all projects,
// sorted by (ProjectUUID, Name).
func (r *volumeRegistry) list() []Volume {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Volume, 0, len(r.byUUID))
	for _, v := range r.byUUID {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CreateVolumeSpec carries the inputs for create(). Struct, not
// positional args, so future optional fields (encryption, IOPS
// cap, source-snapshot UUID) don't churn every call site.
type CreateVolumeSpec struct {
	ProjectUUID string
	Name        string
	SizeGiB     int
	Format      VolumeFormat
}

// create registers a new Volume. Refuses name collisions within
// the project, non-positive size, unknown format.
func (r *volumeRegistry) create(spec CreateVolumeSpec) (Volume, error) {
	if spec.ProjectUUID == "" {
		return Volume{}, fmt.Errorf("empty project_uuid")
	}
	if spec.Name == "" {
		return Volume{}, fmt.Errorf("empty volume name")
	}
	if spec.SizeGiB <= 0 {
		return Volume{}, fmt.Errorf("size_gib must be > 0, got %d", spec.SizeGiB)
	}
	if err := validateVolumeFormat(spec.Format); err != nil {
		return Volume{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := volumeNameKey(spec.ProjectUUID, spec.Name)
	if _, taken := r.nameIdx[key]; taken {
		return Volume{}, fmt.Errorf("volume name %q already in use in project %s", spec.Name, spec.ProjectUUID)
	}
	format := spec.Format
	if format == "" {
		format = VolumeFormatRaw
	}
	v := Volume{
		UUID:        newUUID(),
		ProjectUUID: spec.ProjectUUID,
		Name:        spec.Name,
		SizeGiB:     spec.SizeGiB,
		Format:      format,
		CreatedAt:   time.Now().UTC(),
	}
	r.byUUID[v.UUID] = v
	r.nameIdx[key] = v.UUID
	if _, ok := r.projectIdx[v.ProjectUUID]; !ok {
		r.projectIdx[v.ProjectUUID] = make(map[string]struct{})
	}
	r.projectIdx[v.ProjectUUID][v.UUID] = struct{}{}
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, v.UUID)
		delete(r.nameIdx, key)
		delete(r.projectIdx[v.ProjectUUID], v.UUID)
		if len(r.projectIdx[v.ProjectUUID]) == 0 {
			delete(r.projectIdx, v.ProjectUUID)
		}
		return Volume{}, err
	}
	return v, nil
}

// setName renames within the project. Refuses name collisions.
// (ProjectUUID, UUID, Format) are immutable.
func (r *volumeRegistry) setName(uuid, newName string) error {
	if newName == "" {
		return fmt.Errorf("empty volume name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("volume %q not found", uuid)
	}
	if v.Name == newName {
		return nil
	}
	newKey := volumeNameKey(v.ProjectUUID, newName)
	if existing, taken := r.nameIdx[newKey]; taken && existing != uuid {
		return fmt.Errorf("volume name %q already in use in project %s", newName, v.ProjectUUID)
	}
	delete(r.nameIdx, volumeNameKey(v.ProjectUUID, v.Name))
	v.Name = newName
	r.byUUID[uuid] = v
	r.nameIdx[newKey] = uuid
	return r.saveLocked()
}

// resize updates SizeGiB. Grow-only: refuses shrink to keep the
// guest filesystem from getting truncated under it. The actual
// disk-image extend is a separate concern handled by the storage
// driver — this just records intent.
func (r *volumeRegistry) resize(uuid string, newSizeGiB int) error {
	if newSizeGiB <= 0 {
		return fmt.Errorf("size_gib must be > 0, got %d", newSizeGiB)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("volume %q not found", uuid)
	}
	if newSizeGiB < v.SizeGiB {
		return fmt.Errorf("shrink not supported: current=%d new=%d", v.SizeGiB, newSizeGiB)
	}
	if newSizeGiB == v.SizeGiB {
		return nil
	}
	v.SizeGiB = newSizeGiB
	r.byUUID[uuid] = v
	return r.saveLocked()
}

// attach records that this volume is plugged into the named VM.
// Refuses when the volume is already attached elsewhere — explicit
// detach first.
func (r *volumeRegistry) attach(uuid, vmUUID string) error {
	if vmUUID == "" {
		return fmt.Errorf("empty vm_uuid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("volume %q not found", uuid)
	}
	if v.AttachedTo != "" && v.AttachedTo != vmUUID {
		return fmt.Errorf("volume %q already attached to %s", uuid, v.AttachedTo)
	}
	if v.AttachedTo == vmUUID {
		return nil
	}
	v.AttachedTo = vmUUID
	r.byUUID[uuid] = v
	return r.saveLocked()
}

// detach clears the attachment. No-op when already detached.
func (r *volumeRegistry) detach(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("volume %q not found", uuid)
	}
	if v.AttachedTo == "" {
		return nil
	}
	v.AttachedTo = ""
	r.byUUID[uuid] = v
	return r.saveLocked()
}

// delete drops a volume from the registry. Refuses when still
// attached — explicit detach first.
func (r *volumeRegistry) delete(uuid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byUUID[uuid]
	if !ok {
		return fmt.Errorf("volume %q not found", uuid)
	}
	if v.AttachedTo != "" {
		return fmt.Errorf("volume %q still attached to %s — detach first", uuid, v.AttachedTo)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, volumeNameKey(v.ProjectUUID, v.Name))
	delete(r.projectIdx[v.ProjectUUID], uuid)
	if len(r.projectIdx[v.ProjectUUID]) == 0 {
		delete(r.projectIdx, v.ProjectUUID)
	}
	return r.saveLocked()
}
