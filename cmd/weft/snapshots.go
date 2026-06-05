package main

// snapshots.go implements the four VolumeSnapshot RPCs on top of
// the Adapter's snapshot registry + reflink-cloned blob store
// (pkg/openweft/weft/volumesnapshots.go + snapshotstore.go). Same
// authorisation discipline as the volume handlers in volumes.go:
//
//   * CreateVolumeSnapshot — caller must have access to the parent
//     volume's owning project. Project resolution flows through
//     AuthorizeProject so the parent-volume lookup also acts as
//     the cross-project leak guard.
//   * ListVolumeSnapshots — scoped to the caller's VisibleProjects
//     (filtered post-fetch — same shape as ListVolumes).
//   * RestoreVolumeSnapshot — caller must have access to the
//     snapshot's owning project ; the new volume lands in the same
//     project (no cross-project restore — same shape as a copy of
//     a Volume row).
//   * DeleteVolumeSnapshot — caller must have access to the
//     snapshot's owning project.

import (
	"context"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toVolumeSnapshotInfo lifts the in-process VolumeSnapshot value
// into its wire shape. Mirrors toVolumeInfo (see volumes.go).
func toVolumeSnapshotInfo(s weft.VolumeSnapshot) *weftv1.VolumeSnapshotInfo {
	return &weftv1.VolumeSnapshotInfo{
		Uuid:           s.UUID,
		VolumeUuid:     s.VolumeUUID,
		Name:           s.Name,
		SizeGib:        int64(s.SizeGiB),
		Project:        s.ProjectUUID,
		CreatedAtUnixNs: s.CreatedAt.UnixNano(),
	}
}

// authSnapshot looks up a snapshot + runs the caller through the
// project ACL gate. Same shape as authVolume in volumes.go : the
// existence-leak guard returns PermissionDenied uniformly so the
// "no such snapshot" and "no access" cases are indistinguishable
// from outside.
func (s *weftServer) authSnapshot(ctx context.Context, uuid string) (weft.VolumeSnapshot, error) {
	if uuid == "" {
		return weft.VolumeSnapshot{}, status.Error(codes.InvalidArgument, "uuid is required")
	}
	snap, ok := s.adp.VolumeSnapshotByUUID(uuid)
	if !ok {
		return weft.VolumeSnapshot{}, status.Errorf(codes.PermissionDenied, "no access to snapshot %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, snap.ProjectUUID); err != nil {
		return weft.VolumeSnapshot{}, err
	}
	return snap, nil
}

// CreateVolumeSnapshot reflink-clones the parent volume's image
// blob into a snapshot file + registers a row. The parent's
// existence + project ownership is validated up-front : a stale
// volume_uuid is reported as PermissionDenied to avoid leaking
// other projects' UUIDs.
func (s *weftServer) CreateVolumeSnapshot(ctx context.Context, req *weftv1.CreateVolumeSnapshotRequest) (*weftv1.CreateVolumeSnapshotResponse, error) {
	if req.VolumeUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_uuid is required")
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	parent, ok := s.adp.VolumeByUUID(req.VolumeUuid)
	if !ok {
		// Volume absent : indistinguishable from "no access" so we
		// don't help an attacker enumerate the UUID namespace.
		return nil, status.Errorf(codes.PermissionDenied, "no access to volume %s", req.VolumeUuid)
	}
	// Resolve / authorise the project. Empty request.Project →
	// the parent volume's own project (so the operator doesn't
	// have to restate it).
	wantProject := req.Project
	if wantProject == "" {
		wantProject = parent.ProjectUUID
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, wantProject)
	if err != nil {
		return nil, err
	}
	if projUUID != parent.ProjectUUID {
		return nil, status.Errorf(codes.PermissionDenied, "no access to volume %s", req.VolumeUuid)
	}
	snap, err := s.adp.RegisterVolumeSnapshot(ctx, parent.UUID, req.Name, projUUID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create snapshot: %v", err)
	}
	logger.Printf("CreateVolumeSnapshot uuid=%s name=%s volume=%s project=%s",
		snap.UUID, snap.Name, snap.VolumeUUID, snap.ProjectUUID)
	return &weftv1.CreateVolumeSnapshotResponse{Snapshot: toVolumeSnapshotInfo(snap)}, nil
}

// ListVolumeSnapshots returns every snapshot the caller can see.
// Filtering modes :
//
//   * empty volume_uuid + empty project → caller's VisibleProjects
//     dominate ; admin-shaped callers see everything.
//   * empty volume_uuid + project set → only snapshots in that
//     project (AuthorizeProject guards the cross-project leak).
//   * volume_uuid set → only snapshots from that parent (and the
//     caller must have access to its project).
func (s *weftServer) ListVolumeSnapshots(ctx context.Context, req *weftv1.ListVolumeSnapshotsRequest) (*weftv1.ListVolumeSnapshotsResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	var wantProjectUUID string
	if req.Project != "" {
		uuid, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		wantProjectUUID = uuid
	}
	// A volume_uuid filter implies an access check on its parent.
	var parent weft.Volume
	if req.VolumeUuid != "" {
		v, ok := s.adp.VolumeByUUID(req.VolumeUuid)
		if !ok {
			return nil, status.Errorf(codes.PermissionDenied, "no access to volume %s", req.VolumeUuid)
		}
		if _, err := s.adp.AuthorizeProject(ctx, v.ProjectUUID); err != nil {
			return nil, err
		}
		parent = v
	}
	out := []*weftv1.VolumeSnapshotInfo{}
	for _, snap := range s.adp.VolumeSnapshots() {
		if req.VolumeUuid != "" && snap.VolumeUUID != parent.UUID {
			continue
		}
		if wantProjectUUID != "" && snap.ProjectUUID != wantProjectUUID {
			continue
		}
		if !all {
			if _, ok := visible[snap.ProjectUUID]; !ok {
				continue
			}
		}
		out = append(out, toVolumeSnapshotInfo(snap))
	}
	return &weftv1.ListVolumeSnapshotsResponse{Snapshots: out}, nil
}

// RestoreVolumeSnapshot clones the snapshot's blob into a brand-
// new Volume (same project, same size). The original snapshot +
// parent volume are untouched ; operators wanting in-place
// rollback delete the existing volume + rename the restored one.
func (s *weftServer) RestoreVolumeSnapshot(ctx context.Context, req *weftv1.RestoreVolumeSnapshotRequest) (*weftv1.RestoreVolumeSnapshotResponse, error) {
	if req.NewVolumeName == "" {
		return nil, status.Error(codes.InvalidArgument, "new_volume_name is required")
	}
	snap, err := s.authSnapshot(ctx, req.SnapshotUuid)
	if err != nil {
		return nil, err
	}
	// Optional project narrowing : if the caller spelled one out,
	// it must match the snapshot's own. We don't let RestoreInto
	// cross project boundaries.
	if req.Project != "" {
		projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		if projUUID != snap.ProjectUUID {
			return nil, status.Errorf(codes.PermissionDenied, "no access to snapshot %s", req.SnapshotUuid)
		}
	}
	v, err := s.adp.RestoreVolumeSnapshot(ctx, snap.UUID, req.NewVolumeName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "restore snapshot: %v", err)
	}
	logger.Printf("RestoreVolumeSnapshot snapshot=%s new_volume_uuid=%s name=%s project=%s",
		snap.UUID, v.UUID, v.Name, v.ProjectUUID)
	return &weftv1.RestoreVolumeSnapshotResponse{Volume: toVolumeInfo(v)}, nil
}

// DeleteVolumeSnapshot drops the registry row + unlinks the blob.
// Unknown UUIDs surface as PermissionDenied (same existence-leak
// guard as the other snapshot handlers).
func (s *weftServer) DeleteVolumeSnapshot(ctx context.Context, req *weftv1.DeleteVolumeSnapshotRequest) (*weftv1.DeleteVolumeSnapshotResponse, error) {
	if _, err := s.authSnapshot(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteVolumeSnapshotByUUID(ctx, req.Uuid); err != nil {
		return nil, status.Errorf(codes.Internal, "delete snapshot: %v", err)
	}
	logger.Printf("DeleteVolumeSnapshot uuid=%s", req.Uuid)
	return &weftv1.DeleteVolumeSnapshotResponse{}, nil
}

// RevertVolumeSnapshot rolls the snapshot's parent volume back to
// the captured state. Only supported on block-backend volumes ;
// file-backend parents surface FailedPrecondition. Unknown UUIDs
// surface as PermissionDenied (same existence-leak guard).
func (s *weftServer) RevertVolumeSnapshot(ctx context.Context, req *weftv1.RevertVolumeSnapshotRequest) (*weftv1.RevertVolumeSnapshotResponse, error) {
	if _, err := s.authSnapshot(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.RevertVolumeSnapshotByUUID(ctx, req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "revert snapshot: %v", err)
	}
	logger.Printf("RevertVolumeSnapshot uuid=%s", req.Uuid)
	return &weftv1.RevertVolumeSnapshotResponse{}, nil
}
