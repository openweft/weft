package main

// snapshots_test.go exercises the four VolumeSnapshot gRPC
// handlers directly (no socket round-trip — we build a
// weftServer{adp: adapter} in-process and invoke the methods
// like a unary RPC).
//
// What's pinned :
//   * happy path : Create → List → Restore → Delete
//   * ACL : a mismatched caller produces PermissionDenied
//   * Cross-volume filter on List

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSnapshotStore is the in-test substitute for the reflink
// store : writes a stub file on Create, removes on Delete. Lets
// the handler tests skip every disk-level concern.
type fakeSnapshotStore struct{}

func (fakeSnapshotStore) Create(_ context.Context, parent, snap string) error {
	if parent == "" || snap == "" {
		return errors.New("empty path")
	}
	if err := os.MkdirAll(filepath.Dir(snap), 0o755); err != nil {
		return err
	}
	return os.WriteFile(snap, []byte("snap"), 0o644)
}

func (fakeSnapshotStore) Delete(_ context.Context, snap string) error {
	_ = os.Remove(snap)
	return nil
}

// newSnapshotTestServer builds a real weftServer backed by a
// MemStorage adapter + the fake SnapshotStore. devCaller returns
// a context carrying a Caller{Dev:true} so AuthorizeProject lets
// every project resolution through (sufficient for handler-level
// coverage ; ACL-deny is exercised separately below).
func newSnapshotTestServer(t *testing.T) (*weftServer, *weft.Adapter) {
	t.Helper()
	stateDir := t.TempDir()
	factory := func(string) weft.Storage { return weft.NewMemStorage() }
	adp := weft.NewWithStorage(stateDir, factory).(*weft.Adapter)
	adp.SetSnapshotStore(fakeSnapshotStore{})
	s := &weftServer{adp: adp}
	return s, adp
}

// devCtx returns a context carrying a dev caller (every project
// allowed). Mirrors the misc_test.go pattern from the parent
// package.
func devCtx() context.Context {
	return weft.WithCaller(context.Background(), &weft.Caller{Dev: true})
}

// scopedCtx returns a context carrying a caller authorised on
// `projectUUID` via the per-project group claim (but NOT
// platform-admin nor dev). Used to assert cross-project denies.
func scopedCtx(projectUUID string) context.Context {
	return weft.WithCaller(context.Background(), &weft.Caller{
		Subject: "ldap:scoped-user",
		Issuer:  "https://dex",
		Groups:  []string{weft.ProjectGroup(projectUUID)},
	})
}

// TestCreateVolumeSnapshot_HappyPath : the parent volume must
// exist + the snapshot is registered. The returned wire object
// carries the parent volume's UUID and size.
func TestCreateVolumeSnapshot_HappyPath(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	ctx := devCtx()
	proj, _, _ := adp.CreateProject("snap-test")
	vol, err := adp.CreateVolume(weft.CreateVolumeSpec{
		ProjectUUID: proj.UUID, Name: "data", SizeGiB: 7, Format: weft.VolumeFormatRaw,
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	resp, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{
		VolumeUuid: vol.UUID, Name: "pre-upgrade",
	})
	if err != nil {
		t.Fatalf("CreateVolumeSnapshot: %v", err)
	}
	if resp.Snapshot == nil || resp.Snapshot.VolumeUuid != vol.UUID || resp.Snapshot.Name != "pre-upgrade" {
		t.Errorf("snapshot info wrong: %+v", resp.Snapshot)
	}
	if resp.Snapshot.SizeGib != 7 {
		t.Errorf("snapshot size_gib = %d, want 7", resp.Snapshot.SizeGib)
	}
}

// TestCreateVolumeSnapshot_RejectsEmptyArgs covers the validation
// branches before any registry work.
func TestCreateVolumeSnapshot_RejectsEmptyArgs(t *testing.T) {
	s, _ := newSnapshotTestServer(t)
	ctx := devCtx()
	if _, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{Name: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty volume_uuid → got %v, want InvalidArgument", err)
	}
	if _, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: "u"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty name → got %v, want InvalidArgument", err)
	}
}

// TestCreateVolumeSnapshot_UnknownVolume : a stale volume_uuid
// must surface as PermissionDenied (not NotFound) so the handler
// doesn't leak UUID-existence.
func TestCreateVolumeSnapshot_UnknownVolume(t *testing.T) {
	s, _ := newSnapshotTestServer(t)
	_, err := s.CreateVolumeSnapshot(devCtx(), &weftv1.CreateVolumeSnapshotRequest{
		VolumeUuid: "absent-uuid", Name: "x",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("got %v, want PermissionDenied", err)
	}
}

// TestListVolumeSnapshots_FilterByVolume creates snapshots on two
// parent volumes ; a List request that pins one volume_uuid sees
// only its own snapshots.
func TestListVolumeSnapshots_FilterByVolume(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	ctx := devCtx()
	proj, _, _ := adp.CreateProject("snap-test")
	a, _ := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: proj.UUID, Name: "a", SizeGiB: 5, Format: weft.VolumeFormatRaw})
	b, _ := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: proj.UUID, Name: "b", SizeGiB: 5, Format: weft.VolumeFormatRaw})
	if _, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: a.UUID, Name: "snap-a-1"}); err != nil {
		t.Fatalf("snap-a-1: %v", err)
	}
	if _, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: a.UUID, Name: "snap-a-2"}); err != nil {
		t.Fatalf("snap-a-2: %v", err)
	}
	if _, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: b.UUID, Name: "snap-b-1"}); err != nil {
		t.Fatalf("snap-b-1: %v", err)
	}

	// Unfiltered : all three.
	all, err := s.ListVolumeSnapshots(ctx, &weftv1.ListVolumeSnapshotsRequest{})
	if err != nil {
		t.Fatalf("List(no filter): %v", err)
	}
	if len(all.Snapshots) != 3 {
		t.Errorf("unfiltered list size = %d, want 3", len(all.Snapshots))
	}
	// Filtered to volume a : only the two snap-a-* rows.
	onlyA, err := s.ListVolumeSnapshots(ctx, &weftv1.ListVolumeSnapshotsRequest{VolumeUuid: a.UUID})
	if err != nil {
		t.Fatalf("List(volume=a): %v", err)
	}
	if len(onlyA.Snapshots) != 2 {
		t.Errorf("filtered list size = %d, want 2", len(onlyA.Snapshots))
	}
	for _, snap := range onlyA.Snapshots {
		if snap.VolumeUuid != a.UUID {
			t.Errorf("filtered list leaked snapshot %+v (wrong volume)", snap)
		}
	}
}

// TestRestoreVolumeSnapshot_HappyPath : the new volume name lands
// in the same project, mirrors the snapshot size, and resolves
// via the parent adapter's VolumeByName.
func TestRestoreVolumeSnapshot_HappyPath(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	ctx := devCtx()
	proj, _, _ := adp.CreateProject("snap-test")
	parent, _ := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: proj.UUID, Name: "src", SizeGiB: 11, Format: weft.VolumeFormatRaw})
	snap, err := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: parent.UUID, Name: "pre"})
	if err != nil {
		t.Fatalf("CreateVolumeSnapshot: %v", err)
	}
	resp, err := s.RestoreVolumeSnapshot(ctx, &weftv1.RestoreVolumeSnapshotRequest{
		SnapshotUuid:  snap.Snapshot.Uuid,
		NewVolumeName: "src-restored",
	})
	if err != nil {
		t.Fatalf("RestoreVolumeSnapshot: %v", err)
	}
	if resp.Volume == nil || resp.Volume.Name != "src-restored" || resp.Volume.SizeGib != 11 {
		t.Errorf("restored volume wrong: %+v", resp.Volume)
	}
	if _, ok := adp.VolumeByName(proj.UUID, "src-restored"); !ok {
		t.Errorf("restored volume not in registry")
	}
}

// TestDeleteVolumeSnapshot_HappyPath drops a snapshot and confirms
// the registry reflects the removal.
func TestDeleteVolumeSnapshot_HappyPath(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	ctx := devCtx()
	proj, _, _ := adp.CreateProject("snap-test")
	parent, _ := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: proj.UUID, Name: "x", SizeGiB: 2, Format: weft.VolumeFormatRaw})
	snap, _ := s.CreateVolumeSnapshot(ctx, &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: parent.UUID, Name: "snap"})

	if _, err := s.DeleteVolumeSnapshot(ctx, &weftv1.DeleteVolumeSnapshotRequest{Uuid: snap.Snapshot.Uuid}); err != nil {
		t.Fatalf("DeleteVolumeSnapshot: %v", err)
	}
	if _, ok := adp.VolumeSnapshotByUUID(snap.Snapshot.Uuid); ok {
		t.Errorf("snapshot still in registry after delete")
	}
	// Re-Delete must be PermissionDenied (existence-leak guard) —
	// the row is gone, so authSnapshot can't find it.
	_, err := s.DeleteVolumeSnapshot(ctx, &weftv1.DeleteVolumeSnapshotRequest{Uuid: snap.Snapshot.Uuid})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("second delete: got %v, want PermissionDenied", err)
	}
}

// TestDeleteVolumeSnapshot_DenyMismatchedProject is the
// AuthorizeProject-consulted gate : a caller authorised on a
// different project gets PermissionDenied on every snapshot RPC
// targeting a snapshot they don't own.
func TestDeleteVolumeSnapshot_DenyMismatchedProject(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	// Create the snapshot under a dev caller (owns everything).
	proj, _, _ := adp.CreateProject("snap-owner")
	parent, _ := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: proj.UUID, Name: "x", SizeGiB: 2, Format: weft.VolumeFormatRaw})
	snap, err := s.CreateVolumeSnapshot(devCtx(), &weftv1.CreateVolumeSnapshotRequest{VolumeUuid: parent.UUID, Name: "snap"})
	if err != nil {
		t.Fatalf("CreateVolumeSnapshot: %v", err)
	}
	// Now switch to a caller scoped on a DIFFERENT project.
	scoped := scopedCtx("p-different")
	_, err = s.DeleteVolumeSnapshot(scoped, &weftv1.DeleteVolumeSnapshotRequest{Uuid: snap.Snapshot.Uuid})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("scoped caller: got %v, want PermissionDenied", err)
	}
	// List also filters them out (caller can't see the project,
	// so the row drops out of the response).
	resp, err := s.ListVolumeSnapshots(scoped, &weftv1.ListVolumeSnapshotsRequest{})
	if err != nil {
		t.Fatalf("List under scoped: %v", err)
	}
	for _, got := range resp.Snapshots {
		if got.Uuid == snap.Snapshot.Uuid {
			t.Errorf("scoped caller saw foreign snapshot %s", got.Uuid)
		}
	}
}

// TestCreateVolumeSnapshot_NoCallerInCtx pins the
// AuthorizeProject-consulted guard at the caller-resolution
// level : a context without a Caller errors with Unauthenticated.
func TestCreateVolumeSnapshot_NoCallerInCtx(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	parent, _ := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: "p-1", Name: "x", SizeGiB: 1, Format: weft.VolumeFormatRaw})
	_, err := s.CreateVolumeSnapshot(context.Background(), &weftv1.CreateVolumeSnapshotRequest{
		VolumeUuid: parent.UUID, Name: "n",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("no caller: got %v, want Unauthenticated", err)
	}
}
