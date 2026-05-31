package weft

// volumesnapshots_test.go exercises the snapshot registry +
// Adapter wrappers + reflink-backed SnapshotStore wiring. The
// pure registry / HCL round-trip tests use MemStorage ; the
// Adapter-level tests substitute a fake SnapshotStore so the
// CoW invariant can be asserted without touching real disk
// (the cowclone primitive itself has its own tests).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ── Registry-level tests ──────────────────────────────────────

func TestVolumeSnapshotRegistry_EmptyOnFreshStorage(t *testing.T) {
	reg, err := loadVolumeSnapshotRegistry(context.Background(), NewMemStorage())
	if err != nil {
		t.Fatalf("loadVolumeSnapshotRegistry: %v", err)
	}
	if got := len(reg.byUUID); got != 0 {
		t.Errorf("fresh registry size = %d, want 0", got)
	}
}

func TestVolumeSnapshotRegistry_CreateAndLookup(t *testing.T) {
	reg, _ := loadVolumeSnapshotRegistry(context.Background(), NewMemStorage())
	s := VolumeSnapshot{
		UUID: "snap-1", VolumeUUID: "vol-1", ProjectUUID: "proj-1",
		Name: "alpha", SizeGiB: 10,
	}
	if err := reg.createRow(s); err != nil {
		t.Fatalf("createRow: %v", err)
	}
	got, ok := reg.lookupByUUID("snap-1")
	if !ok || got.Name != "alpha" {
		t.Errorf("lookupByUUID returned %+v, ok=%v", got, ok)
	}
}

func TestVolumeSnapshotRegistry_RejectsCollisions(t *testing.T) {
	reg, _ := loadVolumeSnapshotRegistry(context.Background(), NewMemStorage())
	base := VolumeSnapshot{UUID: "snap-1", VolumeUUID: "vol-1", ProjectUUID: "p", Name: "n", SizeGiB: 5}
	if err := reg.createRow(base); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same UUID — rejected.
	if err := reg.createRow(base); err == nil {
		t.Errorf("duplicate UUID should be rejected")
	}
	// Same (parent, name) — rejected.
	other := base
	other.UUID = "snap-2"
	if err := reg.createRow(other); err == nil {
		t.Errorf("duplicate (volume,name) should be rejected")
	}
}

func TestVolumeSnapshotRegistry_RejectsEmptyFields(t *testing.T) {
	reg, _ := loadVolumeSnapshotRegistry(context.Background(), NewMemStorage())
	cases := []struct {
		name string
		s    VolumeSnapshot
	}{
		{"empty uuid", VolumeSnapshot{VolumeUUID: "v", ProjectUUID: "p", Name: "n", SizeGiB: 1}},
		{"empty volume", VolumeSnapshot{UUID: "u", ProjectUUID: "p", Name: "n", SizeGiB: 1}},
		{"empty project", VolumeSnapshot{UUID: "u", VolumeUUID: "v", Name: "n", SizeGiB: 1}},
		{"empty name", VolumeSnapshot{UUID: "u", VolumeUUID: "v", ProjectUUID: "p", SizeGiB: 1}},
	}
	for _, tc := range cases {
		if err := reg.createRow(tc.s); err == nil {
			t.Errorf("case %q: should be rejected", tc.name)
		}
	}
}

func TestVolumeSnapshotRegistry_ListForVolume(t *testing.T) {
	reg, _ := loadVolumeSnapshotRegistry(context.Background(), NewMemStorage())
	_ = reg.createRow(VolumeSnapshot{UUID: "s1", VolumeUUID: "v1", ProjectUUID: "p", Name: "a", SizeGiB: 5})
	_ = reg.createRow(VolumeSnapshot{UUID: "s2", VolumeUUID: "v1", ProjectUUID: "p", Name: "b", SizeGiB: 5})
	_ = reg.createRow(VolumeSnapshot{UUID: "s3", VolumeUUID: "v2", ProjectUUID: "p", Name: "c", SizeGiB: 5})

	got := reg.listForVolume("v1")
	if len(got) != 2 {
		t.Fatalf("listForVolume(v1) = %d, want 2", len(got))
	}
	if g := reg.listForVolume("absent"); len(g) != 0 {
		t.Errorf("listForVolume(absent) should be empty")
	}
}

func TestVolumeSnapshotRegistry_DeleteRow(t *testing.T) {
	reg, _ := loadVolumeSnapshotRegistry(context.Background(), NewMemStorage())
	_ = reg.createRow(VolumeSnapshot{UUID: "s1", VolumeUUID: "v", ProjectUUID: "p", Name: "n", SizeGiB: 1})
	removed, err := reg.deleteRow("s1")
	if err != nil {
		t.Fatalf("deleteRow: %v", err)
	}
	if removed.UUID != "s1" {
		t.Errorf("removed = %+v", removed)
	}
	if _, ok := reg.lookupByUUID("s1"); ok {
		t.Errorf("snapshot still in registry after delete")
	}
	// Unknown UUID is rejected.
	if _, err := reg.deleteRow("nope"); err == nil {
		t.Errorf("delete of unknown uuid should error")
	}
}

func TestVolumeSnapshotRegistry_RoundTrip(t *testing.T) {
	storage := NewMemStorage()
	reg, _ := loadVolumeSnapshotRegistry(context.Background(), storage)
	_ = reg.createRow(VolumeSnapshot{
		UUID: "snap-1", VolumeUUID: "vol-1", ProjectUUID: "proj-1",
		Name: "before-upgrade", SizeGiB: 50,
	})
	blob, _ := storage.Load(context.Background())
	if !strings.Contains(string(blob), "snap-1") || !strings.Contains(string(blob), "before-upgrade") {
		t.Errorf("HCL missing fields: %s", blob)
	}
	reg2, err := loadVolumeSnapshotRegistry(context.Background(), storage)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	got, ok := reg2.lookupByUUID("snap-1")
	if !ok || got.Name != "before-upgrade" || got.SizeGiB != 50 {
		t.Errorf("re-loaded snapshot wrong: %+v", got)
	}
}

// ── SnapshotStore tests ────────────────────────────────────────

// TestReflinkSnapshotStore_CreateAndDelete exercises the real
// cowclone-backed store : clone a synthetic parent file into a
// snapshot path, confirm the bytes match (CoW or fallback copy —
// both honour the same observable contract), then Delete.
func TestReflinkSnapshotStore_CreateAndDelete(t *testing.T) {
	parentDir := t.TempDir()
	snapDir := t.TempDir()
	parent := filepath.Join(parentDir, "vol-1.bin")
	want := []byte("parent volume image bytes")
	if err := os.WriteFile(parent, want, 0o644); err != nil {
		t.Fatalf("write parent: %v", err)
	}

	store := NewReflinkSnapshotStore(snapDir)
	snapPath := snapshotPathIn(snapDir, "snap-1")
	if err := store.Create(context.Background(), parent, snapPath); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("snapshot bytes differ: got %q, want %q", got, want)
	}
	if err := store.Delete(context.Background(), snapPath); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(snapPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("snapshot file should be gone after Delete, got err=%v", err)
	}
	// Re-Delete is idempotent (registry-first ordering depends on this).
	if err := store.Delete(context.Background(), snapPath); err != nil {
		t.Errorf("second Delete should be a no-op: %v", err)
	}
}

func TestReflinkSnapshotStore_CreateRejectsEmpty(t *testing.T) {
	store := NewReflinkSnapshotStore(t.TempDir())
	if err := store.Create(context.Background(), "", "/tmp/snap"); err == nil {
		t.Errorf("empty parent path should error")
	}
	if err := store.Create(context.Background(), "/tmp/parent", ""); err == nil {
		t.Errorf("empty snapshot path should error")
	}
}

// ── Adapter-level tests with a fake SnapshotStore ─────────────

// fakeSnapshotStore records every Create/Delete call. It writes a
// small stub file on Create so the registry-first ordering and
// path-derivation can both be asserted.
type fakeSnapshotStore struct {
	mu        sync.Mutex
	creates   []fakeSnapshotCall
	deletes   []string
	failCreate error
	failDelete error
}

type fakeSnapshotCall struct{ parent, snapshot string }

func (f *fakeSnapshotStore) Create(ctx context.Context, parent, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate != nil {
		return f.failCreate
	}
	f.creates = append(f.creates, fakeSnapshotCall{parent: parent, snapshot: snap})
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		return err
	}
	return os.WriteFile(snap, []byte("snap"), 0o644)
}

func (f *fakeSnapshotStore) Delete(ctx context.Context, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete != nil {
		return f.failDelete
	}
	f.deletes = append(f.deletes, snap)
	_ = os.Remove(snap)
	return nil
}

// snapshotAdapterT builds an Adapter with MemStorage + a
// fakeSnapshotStore wired in. Returns both so tests can assert
// on the SnapshotStore call log.
func snapshotAdapterT(t *testing.T) (*Adapter, *fakeSnapshotStore) {
	t.Helper()
	stateDir := t.TempDir()
	factory := func(string) Storage { return NewMemStorage() }
	a := NewWithStorage(stateDir, factory).(*Adapter)
	fake := &fakeSnapshotStore{}
	a.SetSnapshotStore(fake)
	return a, fake
}

// TestAdapter_VolumeSnapshot_CreateListRestoreDelete is the
// table-driven happy-path : create two snapshots, list, restore
// one into a new volume, delete the other.
func TestAdapter_VolumeSnapshot_CreateListRestoreDelete(t *testing.T) {
	a, fake := snapshotAdapterT(t)
	parent, err := a.CreateVolume(CreateVolumeSpec{
		ProjectUUID: "p-1", Name: "data", SizeGiB: 20, Format: VolumeFormatRaw,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}

	// Create snapshot 1.
	s1, err := a.RegisterVolumeSnapshot(context.Background(), parent.UUID, "before-upgrade", "p-1")
	if err != nil {
		t.Fatalf("RegisterVolumeSnapshot 1: %v", err)
	}
	if s1.VolumeUUID != parent.UUID || s1.SizeGiB != 20 || s1.ProjectUUID != "p-1" {
		t.Errorf("snapshot 1 fields wrong: %+v", s1)
	}
	// Create snapshot 2.
	s2, err := a.RegisterVolumeSnapshot(context.Background(), parent.UUID, "after-upgrade", "p-1")
	if err != nil {
		t.Fatalf("RegisterVolumeSnapshot 2: %v", err)
	}

	// List for-volume returns both.
	got := a.ListVolumeSnapshotsForVolume(parent.UUID)
	if len(got) != 2 {
		t.Errorf("ListVolumeSnapshotsForVolume = %d, want 2", len(got))
	}
	// List for-project also returns both.
	if gp := a.ListVolumeSnapshotsForProject("p-1"); len(gp) != 2 {
		t.Errorf("ListVolumeSnapshotsForProject(p-1) = %d, want 2", len(gp))
	}
	// List for-project of an unrelated project is empty.
	if gp := a.ListVolumeSnapshotsForProject("p-other"); len(gp) != 0 {
		t.Errorf("ListVolumeSnapshotsForProject(p-other) = %d, want 0", len(gp))
	}

	// Both creates went through the fake.
	if len(fake.creates) != 2 {
		t.Fatalf("fake.creates = %d, want 2", len(fake.creates))
	}
	wantParentPath := a.volumePath(parent.UUID)
	if fake.creates[0].parent != wantParentPath {
		t.Errorf("create.parent = %q, want %q", fake.creates[0].parent, wantParentPath)
	}

	// Restore snapshot 1 into a new volume.
	v, err := a.RestoreVolumeSnapshot(context.Background(), s1.UUID, "restored-data")
	if err != nil {
		t.Fatalf("RestoreVolumeSnapshot: %v", err)
	}
	if v.Name != "restored-data" || v.SizeGiB != 20 || v.ProjectUUID != "p-1" {
		t.Errorf("restored volume fields wrong: %+v", v)
	}
	// Restore clones the snapshot's blob → the new volume's blob path.
	lastCreate := fake.creates[len(fake.creates)-1]
	if lastCreate.parent != a.snapshotPath(s1.UUID) {
		t.Errorf("restore parent path = %q, want snapshot path %q",
			lastCreate.parent, a.snapshotPath(s1.UUID))
	}
	if lastCreate.snapshot != a.volumePath(v.UUID) {
		t.Errorf("restore dst path = %q, want volume path %q",
			lastCreate.snapshot, a.volumePath(v.UUID))
	}

	// Delete snapshot 2.
	if err := a.DeleteVolumeSnapshotByUUID(context.Background(), s2.UUID); err != nil {
		t.Fatalf("DeleteVolumeSnapshotByUUID: %v", err)
	}
	if _, ok := a.VolumeSnapshotByUUID(s2.UUID); ok {
		t.Errorf("snapshot 2 still resolves after delete")
	}
	if len(fake.deletes) != 1 || fake.deletes[0] != a.snapshotPath(s2.UUID) {
		t.Errorf("fake.deletes = %v, want [%s]", fake.deletes, a.snapshotPath(s2.UUID))
	}
}

// TestAdapter_RegisterVolumeSnapshot_RollsBackOnCloneFailure
// pins the registry-first ordering's safety net : if the clone
// step blows up, the row must NOT be left behind.
func TestAdapter_RegisterVolumeSnapshot_RollsBackOnCloneFailure(t *testing.T) {
	a, fake := snapshotAdapterT(t)
	parent, _ := a.CreateVolume(CreateVolumeSpec{
		ProjectUUID: "p-1", Name: "data", SizeGiB: 5, Format: VolumeFormatRaw,
	})
	fake.failCreate = errors.New("simulated reflink failure")
	if _, err := a.RegisterVolumeSnapshot(context.Background(), parent.UUID, "snap", "p-1"); err == nil {
		t.Fatal("expected RegisterVolumeSnapshot to fail")
	}
	// Registry must be empty — no phantom row.
	if got := a.ListVolumeSnapshotsForVolume(parent.UUID); len(got) != 0 {
		t.Errorf("registry has %d snapshots after rollback, want 0: %+v", len(got), got)
	}
}

// TestAdapter_RegisterVolumeSnapshot_RejectsUnknownParent : the
// parent UUID must resolve before any blob work happens.
func TestAdapter_RegisterVolumeSnapshot_RejectsUnknownParent(t *testing.T) {
	a, fake := snapshotAdapterT(t)
	if _, err := a.RegisterVolumeSnapshot(context.Background(), "unknown-uuid", "snap", "p-1"); err == nil {
		t.Fatal("unknown parent should error")
	}
	if len(fake.creates) != 0 {
		t.Errorf("fake.creates = %d, want 0 (parent check should short-circuit)", len(fake.creates))
	}
}

// TestAdapter_RegisterVolumeSnapshot_CrossProjectRejected :
// supplying a projectUUID that doesn't own the parent is refused.
func TestAdapter_RegisterVolumeSnapshot_CrossProjectRejected(t *testing.T) {
	a, _ := snapshotAdapterT(t)
	parent, _ := a.CreateVolume(CreateVolumeSpec{
		ProjectUUID: "p-1", Name: "data", SizeGiB: 5, Format: VolumeFormatRaw,
	})
	if _, err := a.RegisterVolumeSnapshot(context.Background(), parent.UUID, "snap", "p-other"); err == nil {
		t.Fatal("cross-project should error")
	}
}

// TestAdapter_RestoreVolumeSnapshot_RollsBackOnCloneFailure :
// the restored Volume row is removed if the clone step fails.
func TestAdapter_RestoreVolumeSnapshot_RollsBackOnCloneFailure(t *testing.T) {
	a, fake := snapshotAdapterT(t)
	parent, _ := a.CreateVolume(CreateVolumeSpec{
		ProjectUUID: "p-1", Name: "data", SizeGiB: 5, Format: VolumeFormatRaw,
	})
	s, err := a.RegisterVolumeSnapshot(context.Background(), parent.UUID, "snap", "p-1")
	if err != nil {
		t.Fatalf("RegisterVolumeSnapshot: %v", err)
	}
	fake.failCreate = errors.New("simulated reflink failure")
	if _, err := a.RestoreVolumeSnapshot(context.Background(), s.UUID, "restored"); err == nil {
		t.Fatal("expected RestoreVolumeSnapshot to fail")
	}
	// The "restored" volume row must NOT be in the registry.
	if _, ok := a.VolumeByName("p-1", "restored"); ok {
		t.Errorf("rolled-back volume still in registry")
	}
}
