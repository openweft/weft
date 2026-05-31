package weft

// volumesnapshots.go owns the per-project block-volume snapshot
// registry + the reflink-backed on-disk blob workflow. A snapshot
// is a CoW copy of a parent volume's image, captured atomically at
// the moment of creation and restorable into a new volume.
//
// Pattern mirrors volumes.go (same multi-tenant naming, same HCL
// schema discipline, same blob-via-Storage atomic save). The
// twist: every mutation that touches a snapshot also touches a
// blob on disk via SnapshotStore. Ordering is registry-first :
//
//   * Create — write the registry row first, then reflink the blob.
//     A failed clone after a successful save leaves a row pointing
//     at a missing file ; the operator can re-run Delete to clean
//     up. The inverse (blob first, row later) would leak orphaned
//     files invisible to weft's bookkeeping.
//
//   * Delete — drop the row first, then unlink the blob. A missing
//     blob is fine (idempotent unlink). A leaked blob from a half-
//     failed prior delete is recoverable by re-running with the
//     blob path ; that's a feature, not a bug.
//
// Schema:
//
//   volume_snapshot "snap-abc-…" {
//     volume_uuid  = "vol-…"
//     project_uuid = "p-…"
//     name         = "before-upgrade"
//     size_gib     = 100
//     created_at   = "..."
//   }

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// VolumeSnapshot is one row in the snapshot registry. UUID is the
// snapshot's own identity ; VolumeUUID pins the parent volume.
// SizeGiB is captured at snapshot-time so RestoreVolumeSnapshot
// can mint a new Volume row without re-querying the parent.
type VolumeSnapshot struct {
	UUID        string    `json:"uuid"`
	VolumeUUID  string    `json:"volume_uuid"`
	ProjectUUID string    `json:"project_uuid"`
	Name        string    `json:"name"`
	SizeGiB     int       `json:"size_gib"`
	CreatedAt   time.Time `json:"created_at"`
}

// volumeSnapshotsDoc is the top-level HCL schema for the snapshot
// registry blob ; one volume_snapshot block per entry.
type volumeSnapshotsDoc struct {
	Snapshots []volumeSnapshotBlock `hcl:"volume_snapshot,block"`
}

type volumeSnapshotBlock struct {
	UUID        string `hcl:",label"`
	VolumeUUID  string `hcl:"volume_uuid"`
	ProjectUUID string `hcl:"project_uuid"`
	Name        string `hcl:"name"`
	SizeGiB     int    `hcl:"size_gib"`
	CreatedAt   string `hcl:"created_at"`
}

// volumeSnapshotRegistry indexes snapshots by UUID, by
// (volumeUUID,name) for collision detection, and by parent volume
// UUID for List filtering.
type volumeSnapshotRegistry struct {
	mu        sync.Mutex
	storage   Storage
	byUUID    map[string]VolumeSnapshot
	nameIdx   map[string]string              // (volumeUUID,name) → snapshot UUID
	volumeIdx map[string]map[string]struct{} // volumeUUID → set-of-snapshot-UUIDs
}

// loadVolumeSnapshotRegistry reads the registry blob via Storage.
// Absent / empty blob yields an empty registry.
func loadVolumeSnapshotRegistry(ctx context.Context, storage Storage) (*volumeSnapshotRegistry, error) {
	reg := &volumeSnapshotRegistry{
		storage:   storage,
		byUUID:    make(map[string]VolumeSnapshot),
		nameIdx:   make(map[string]string),
		volumeIdx: make(map[string]map[string]struct{}),
	}
	blob, err := storage.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load volume-snapshot registry: %w", err)
	}
	if len(blob) == 0 {
		return reg, nil
	}
	var doc volumeSnapshotsDoc
	if err := hclsimple.Decode("volume-snapshot-registry.hcl", blob, nil, &doc); err != nil {
		return nil, fmt.Errorf("parse volume-snapshot registry: %w", err)
	}
	for _, b := range doc.Snapshots {
		created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		s := VolumeSnapshot{
			UUID:        b.UUID,
			VolumeUUID:  b.VolumeUUID,
			ProjectUUID: b.ProjectUUID,
			Name:        b.Name,
			SizeGiB:     b.SizeGiB,
			CreatedAt:   created,
		}
		reg.byUUID[s.UUID] = s
		reg.nameIdx[volumeSnapshotNameKey(s.VolumeUUID, s.Name)] = s.UUID
		if _, ok := reg.volumeIdx[s.VolumeUUID]; !ok {
			reg.volumeIdx[s.VolumeUUID] = make(map[string]struct{})
		}
		reg.volumeIdx[s.VolumeUUID][s.UUID] = struct{}{}
	}
	return reg, nil
}

// volumeSnapshotNameKey composes the secondary index key. NUL
// separator: neither UUIDs nor names contain it.
func volumeSnapshotNameKey(volumeUUID, name string) string {
	return volumeUUID + "\x00" + name
}

// saveLocked encodes the registry as HCL and atomically persists
// via Storage. Caller holds mu. Output is UUID-sorted for stable
// diffs across runs.
func (r *volumeSnapshotRegistry) saveLocked() error {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	body.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type: 0,
		Bytes: []byte(
			"# weft volume-snapshot registry — UUID-keyed per [[weft-uuid-keyed-resources]].\n" +
				"# Never edit `volume_uuid`, `project_uuid`, `size_gib`, or the block\n" +
				"# label (UUID). `name` may be edited freely (unique per parent).\n\n",
		),
	}})
	uuids := make([]string, 0, len(r.byUUID))
	for u := range r.byUUID {
		uuids = append(uuids, u)
	}
	sort.Strings(uuids)
	for _, u := range uuids {
		s := r.byUUID[u]
		block := body.AppendNewBlock("volume_snapshot", []string{u})
		bb := block.Body()
		bb.SetAttributeValue("volume_uuid", cty.StringVal(s.VolumeUUID))
		bb.SetAttributeValue("project_uuid", cty.StringVal(s.ProjectUUID))
		bb.SetAttributeValue("name", cty.StringVal(s.Name))
		bb.SetAttributeValue("size_gib", cty.NumberIntVal(int64(s.SizeGiB)))
		bb.SetAttributeValue("created_at", cty.StringVal(s.CreatedAt.Format(time.RFC3339Nano)))
		body.AppendNewline()
	}
	return r.storage.Save(context.Background(), f.Bytes())
}

// lookupByUUID returns (snapshot, true) when the UUID is known.
func (r *volumeSnapshotRegistry) lookupByUUID(uuid string) (VolumeSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byUUID[uuid]
	return s, ok
}

// listForVolume returns every snapshot taken from the named parent
// volume, sorted by CreatedAt (oldest first — matches typical UI).
func (r *volumeSnapshotRegistry) listForVolume(volumeUUID string) []VolumeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	uuids := r.volumeIdx[volumeUUID]
	if len(uuids) == 0 {
		return nil
	}
	out := make([]VolumeSnapshot, 0, len(uuids))
	for u := range uuids {
		out = append(out, r.byUUID[u])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// listForProject returns every snapshot owned by `projectUUID`,
// across all parent volumes. Sorted by (VolumeUUID, CreatedAt).
func (r *volumeSnapshotRegistry) listForProject(projectUUID string) []VolumeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []VolumeSnapshot
	for _, s := range r.byUUID {
		if s.ProjectUUID == projectUUID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VolumeUUID != out[j].VolumeUUID {
			return out[i].VolumeUUID < out[j].VolumeUUID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// list returns every snapshot across every project, sorted by
// (ProjectUUID, VolumeUUID, CreatedAt). Used by the gRPC handler
// when filtering happens above (visibility scoping).
func (r *volumeSnapshotRegistry) list() []VolumeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]VolumeSnapshot, 0, len(r.byUUID))
	for _, s := range r.byUUID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectUUID != out[j].ProjectUUID {
			return out[i].ProjectUUID < out[j].ProjectUUID
		}
		if out[i].VolumeUUID != out[j].VolumeUUID {
			return out[i].VolumeUUID < out[j].VolumeUUID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// createRow registers a fresh snapshot row. Rejects empty inputs +
// (parent,name) collisions ; rolls back the in-memory mutation on
// a Storage.Save failure.
func (r *volumeSnapshotRegistry) createRow(s VolumeSnapshot) error {
	if s.UUID == "" {
		return fmt.Errorf("empty snapshot uuid")
	}
	if s.VolumeUUID == "" {
		return fmt.Errorf("empty volume_uuid")
	}
	if s.ProjectUUID == "" {
		return fmt.Errorf("empty project_uuid")
	}
	if s.Name == "" {
		return fmt.Errorf("empty snapshot name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byUUID[s.UUID]; taken {
		return fmt.Errorf("snapshot uuid %q already exists", s.UUID)
	}
	key := volumeSnapshotNameKey(s.VolumeUUID, s.Name)
	if _, taken := r.nameIdx[key]; taken {
		return fmt.Errorf("snapshot name %q already in use for volume %s", s.Name, s.VolumeUUID)
	}
	r.byUUID[s.UUID] = s
	r.nameIdx[key] = s.UUID
	if _, ok := r.volumeIdx[s.VolumeUUID]; !ok {
		r.volumeIdx[s.VolumeUUID] = make(map[string]struct{})
	}
	r.volumeIdx[s.VolumeUUID][s.UUID] = struct{}{}
	if err := r.saveLocked(); err != nil {
		delete(r.byUUID, s.UUID)
		delete(r.nameIdx, key)
		delete(r.volumeIdx[s.VolumeUUID], s.UUID)
		if len(r.volumeIdx[s.VolumeUUID]) == 0 {
			delete(r.volumeIdx, s.VolumeUUID)
		}
		return err
	}
	return nil
}

// deleteRow drops the snapshot row by UUID. Returns the removed
// entry so the caller can chain a blob unlink without a re-lookup.
// Unknown UUID is rejected (callers want feedback for typos).
func (r *volumeSnapshotRegistry) deleteRow(uuid string) (VolumeSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byUUID[uuid]
	if !ok {
		return VolumeSnapshot{}, fmt.Errorf("snapshot %q not found", uuid)
	}
	delete(r.byUUID, uuid)
	delete(r.nameIdx, volumeSnapshotNameKey(s.VolumeUUID, s.Name))
	delete(r.volumeIdx[s.VolumeUUID], uuid)
	if len(r.volumeIdx[s.VolumeUUID]) == 0 {
		delete(r.volumeIdx, s.VolumeUUID)
	}
	if err := r.saveLocked(); err != nil {
		// Roll back the in-memory removal so the registry stays
		// consistent if Storage rejected the save.
		r.byUUID[s.UUID] = s
		r.nameIdx[volumeSnapshotNameKey(s.VolumeUUID, s.Name)] = s.UUID
		if _, ok := r.volumeIdx[s.VolumeUUID]; !ok {
			r.volumeIdx[s.VolumeUUID] = make(map[string]struct{})
		}
		r.volumeIdx[s.VolumeUUID][s.UUID] = struct{}{}
		return VolumeSnapshot{}, err
	}
	return s, nil
}

// ── Adapter surface ────────────────────────────────────────────

// SnapshotsDir is the on-disk root holding every snapshot blob.
// Layout: <SnapshotsDir>/<snapshot-uuid>.bin — see snapshotstore.go.
func (a *Adapter) SnapshotsDir() string {
	return filepath.Join(a.stateDir, "snapshots")
}

// VolumesDir is the on-disk root holding every volume image blob.
// Convention is mirror of SnapshotsDir : <VolumesDir>/<uuid>.bin.
// Treated as the parent path the SnapshotStore reads from on a
// Create call ; for tests that haven't materialised a real image,
// callers can stub via SetVolumesDir.
func (a *Adapter) VolumesDir() string {
	if a.volumesDir != "" {
		return a.volumesDir
	}
	return filepath.Join(a.stateDir, "volumes")
}

// SetVolumesDir overrides the default <stateDir>/volumes path.
// Used by tests that materialise the parent image somewhere other
// than the adapter's stateDir (e.g. t.TempDir()).
func (a *Adapter) SetVolumesDir(dir string) { a.volumesDir = dir }

// SetSnapshotStore overrides the default reflink-backed store with
// a custom one (tests substitute an in-memory fake to assert
// ordering invariants without touching disk).
func (a *Adapter) SetSnapshotStore(s SnapshotStore) { a.snapshotStore = s }

// volumePath returns the on-disk path of the volume's image blob.
// The convention <VolumesDir>/<uuid>.bin matches the snapshot
// shape — both layers can use SnapshotStore.Create without a
// path-translation table.
func (a *Adapter) volumePath(uuid string) string {
	return filepath.Join(a.VolumesDir(), uuid+snapshotFileExt)
}

// snapshotPath returns the on-disk path of the snapshot's blob.
func (a *Adapter) snapshotPath(uuid string) string {
	return snapshotPathIn(a.SnapshotsDir(), uuid)
}

// VolumeSnapshotByUUID is the read-side lookup used by both ACL
// pre-checks (resolve a snapshot's owning project) and the Delete
// handler.
func (a *Adapter) VolumeSnapshotByUUID(uuid string) (VolumeSnapshot, bool) {
	if a.snapshotReg == nil {
		return VolumeSnapshot{}, false
	}
	return a.snapshotReg.lookupByUUID(uuid)
}

// ListVolumeSnapshotsForVolume returns snapshots taken from the
// named parent volume. Empty volumeUUID falls back to listForProject.
func (a *Adapter) ListVolumeSnapshotsForVolume(volumeUUID string) []VolumeSnapshot {
	if a.snapshotReg == nil {
		return nil
	}
	return a.snapshotReg.listForVolume(volumeUUID)
}

// ListVolumeSnapshotsForProject returns every snapshot owned by
// the project, regardless of parent volume. The gRPC handler uses
// this when no volume_uuid filter is supplied.
func (a *Adapter) ListVolumeSnapshotsForProject(projectUUID string) []VolumeSnapshot {
	if a.snapshotReg == nil {
		return nil
	}
	return a.snapshotReg.listForProject(projectUUID)
}

// VolumeSnapshots returns every snapshot in the registry across
// every project — the unscoped accessor, used by tests + the
// admin-style ACL filter path in the List handler.
func (a *Adapter) VolumeSnapshots() []VolumeSnapshot {
	if a.snapshotReg == nil {
		return nil
	}
	return a.snapshotReg.list()
}

// RegisterVolumeSnapshot is the snapshot-create entry point used
// by the gRPC handler. Ordering: write the registry row first
// (so any orphan blob from a half-failed Create is recoverable
// via Delete), then reflink the parent volume's image. The parent
// volume's existence + project ownership is checked by the
// caller (gRPC handler runs AuthorizeProject on the parent's
// ProjectUUID).
func (a *Adapter) RegisterVolumeSnapshot(ctx context.Context, parentVolumeUUID, name, projectUUID string) (VolumeSnapshot, error) {
	if parentVolumeUUID == "" {
		return VolumeSnapshot{}, fmt.Errorf("empty parent volume uuid")
	}
	parent, ok := a.VolumeByUUID(parentVolumeUUID)
	if !ok {
		return VolumeSnapshot{}, fmt.Errorf("parent volume %q not found", parentVolumeUUID)
	}
	if projectUUID != "" && parent.ProjectUUID != projectUUID {
		return VolumeSnapshot{}, fmt.Errorf("parent volume %q not in project %q", parentVolumeUUID, projectUUID)
	}
	s := VolumeSnapshot{
		UUID:        newUUID(),
		VolumeUUID:  parent.UUID,
		ProjectUUID: parent.ProjectUUID,
		Name:        name,
		SizeGiB:     parent.SizeGiB,
		CreatedAt:   time.Now().UTC(),
	}
	if err := a.snapshotReg.createRow(s); err != nil {
		return VolumeSnapshot{}, err
	}
	// Row persisted ; now materialise the blob. On clone failure
	// we roll the row back so the operator doesn't see a phantom
	// snapshot — the registry-first ordering is meant for crashes,
	// not for an immediate Create-time failure.
	dst := a.snapshotPath(s.UUID)
	if err := a.snapshotStoreOrDefault().Create(ctx, a.volumePath(parent.UUID), dst); err != nil {
		if _, rbErr := a.snapshotReg.deleteRow(s.UUID); rbErr != nil {
			return VolumeSnapshot{}, fmt.Errorf("clone snapshot: %w (rollback also failed: %v)", err, rbErr)
		}
		return VolumeSnapshot{}, fmt.Errorf("clone snapshot: %w", err)
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "volume_snapshot.created",
		Subject:     s.UUID,
		ProjectUUID: s.ProjectUUID,
		Meta: map[string]string{
			"name":        s.Name,
			"volume_uuid": s.VolumeUUID,
			"size_gib":    fmt.Sprintf("%d", s.SizeGiB),
		},
	})
	return s, nil
}

// RestoreVolumeSnapshot clones the snapshot's blob into a fresh
// volume (CoW) + registers a matching Volume row. The parent
// volume is untouched ; the new volume lives in the same project
// as the snapshot. Returns the newly minted Volume.
func (a *Adapter) RestoreVolumeSnapshot(ctx context.Context, snapshotUUID, newVolumeName string) (Volume, error) {
	if snapshotUUID == "" {
		return Volume{}, fmt.Errorf("empty snapshot uuid")
	}
	if newVolumeName == "" {
		return Volume{}, fmt.Errorf("empty new_volume_name")
	}
	s, ok := a.VolumeSnapshotByUUID(snapshotUUID)
	if !ok {
		return Volume{}, fmt.Errorf("snapshot %q not found", snapshotUUID)
	}
	// Register the new volume first — same registry-first ordering
	// the snapshot Create path uses, for the same reason.
	v, err := a.volumeReg.create(CreateVolumeSpec{
		ProjectUUID: s.ProjectUUID,
		Name:        newVolumeName,
		SizeGiB:     s.SizeGiB,
		Format:      VolumeFormatRaw,
	})
	if err != nil {
		return Volume{}, fmt.Errorf("register restored volume: %w", err)
	}
	dst := a.volumePath(v.UUID)
	src := a.snapshotPath(s.UUID)
	if err := a.snapshotStoreOrDefault().Create(ctx, src, dst); err != nil {
		// Roll back the volume row so the operator can retry
		// without a stale entry blocking the new name.
		if delErr := a.volumeReg.delete(v.UUID); delErr != nil {
			return Volume{}, fmt.Errorf("restore snapshot: %w (rollback failed: %v)", err, delErr)
		}
		return Volume{}, fmt.Errorf("restore snapshot: %w", err)
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "volume_snapshot.restored",
		Subject:     v.UUID,
		ProjectUUID: v.ProjectUUID,
		Meta: map[string]string{
			"name":          v.Name,
			"snapshot_uuid": s.UUID,
			"size_gib":      fmt.Sprintf("%d", v.SizeGiB),
		},
	})
	return v, nil
}

// DeleteVolumeSnapshotByUUID is the snapshot-delete entry point :
// drop the row first, then unlink the blob. The unlink is
// idempotent so a half-failed prior call can be replayed.
func (a *Adapter) DeleteVolumeSnapshotByUUID(ctx context.Context, uuid string) error {
	if uuid == "" {
		return fmt.Errorf("empty snapshot uuid")
	}
	s, err := a.snapshotReg.deleteRow(uuid)
	if err != nil {
		return err
	}
	if err := a.snapshotStoreOrDefault().Delete(ctx, a.snapshotPath(uuid)); err != nil {
		// Best-effort : the row is gone, so the operator can
		// re-run a cleanup via the snapshot path manually. Don't
		// rehydrate the row — that would defeat the registry-first
		// ordering's point (leaked blob, recoverable ; phantom
		// row pointing at a missing file, not).
		return fmt.Errorf("unlink snapshot blob: %w", err)
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "volume_snapshot.deleted",
		Subject:     uuid,
		ProjectUUID: s.ProjectUUID,
	})
	return nil
}

// snapshotStoreOrDefault returns the configured SnapshotStore,
// constructing a reflink-backed one rooted at SnapshotsDir on
// first use. Lazily initialised so the Adapter constructor doesn't
// have to know about it.
func (a *Adapter) snapshotStoreOrDefault() SnapshotStore {
	if a.snapshotStore != nil {
		return a.snapshotStore
	}
	a.snapshotStore = NewReflinkSnapshotStore(a.SnapshotsDir())
	return a.snapshotStore
}

// initVolumeSnapshots loads the registry blob via storageFactory.
// Failure to load downgrades to an empty registry — same
// resilience contract as initVolumes.
func (a *Adapter) initVolumeSnapshots() {
	storage := a.storageFactory("volume_snapshots")
	reg, err := loadVolumeSnapshotRegistry(context.Background(), storage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load volume-snapshot registry: %v\n", err)
		reg = &volumeSnapshotRegistry{
			storage:   storage,
			byUUID:    make(map[string]VolumeSnapshot),
			nameIdx:   make(map[string]string),
			volumeIdx: make(map[string]map[string]struct{}),
		}
	}
	a.snapshotReg = reg
}
