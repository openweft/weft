package main

// backups.go implements the four VolumeBackup RPCs on top of the
// Adapter's block-driver dispatch (pkg/openweft/weft/volumebackups.go).
// Same authorisation discipline as the snapshot handlers: the caller
// must have access to the snapshot's owning project for CreateBackup,
// to the requesting project for List, and to the volume the URL came
// from for Delete/Restore (resolved via ListVolumeBackups round-trip).
//
// Targets we ship (in preference order) :
//   - oci://<registry>/<repo>:<tag>      — content-addressed, mirror-friendly
//   - s3://<bucket>@<region>/<prefix>    — versitygw / CubeFS objectnode
//   - sftp://<user>@<host>:<port>/<path> — sftpgo / OpenSSH sshd
//   - fs:///<absolute_path>              — dev / tests only
//
// Encryption-at-rest (AEAD + KDF) lives inside weft-block — the
// daemon reads WEFT_BACKUP_PASSPHRASE / WEFT_BACKUP_PASSPHRASE_ENV at
// the driver layer. The control plane only forwards target URLs.

import (
	"context"

	drivers "github.com/openweft/weft-drivers"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toVolumeBackupInfo lifts a drivers.Backup descriptor into its
// wire shape. URL is the addressing key — Delete + Restore take it
// back, so callers should treat it as opaque + persistent.
func toVolumeBackupInfo(bk drivers.Backup, project string) *weftv1.VolumeBackupInfo {
	return &weftv1.VolumeBackupInfo{
		Url:             bk.URL,
		VolumeUuid:      bk.VolumeUUID,
		SnapshotUuid:    bk.SnapshotName, // weft writes UUID into SnapshotName
		Project:         project,
		SizeBytes:       bk.SizeBytes,
		CreatedAtUnixNs: bk.CreatedAtUnixNs,
		State:           bk.State,
		Error:           bk.Error,
	}
}

// CreateVolumeBackup ships the snapshot to the operator's target URL.
// The snapshot's parent volume must be block-backed (file backend has
// no off-host backup story yet — file → FailedPrecondition).
func (s *weftServer) CreateVolumeBackup(ctx context.Context, req *weftv1.CreateVolumeBackupRequest) (*weftv1.CreateVolumeBackupResponse, error) {
	if req.SnapshotUuid == "" {
		return nil, status.Errorf(codes.InvalidArgument, "snapshot_uuid is required")
	}
	if req.Target == "" {
		return nil, status.Errorf(codes.InvalidArgument, "target is required")
	}
	snap, err := s.authSnapshot(ctx, req.SnapshotUuid)
	if err != nil {
		return nil, err
	}
	bk, err := s.adp.CreateVolumeBackup(ctx, snap.UUID, req.Target)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "create backup: %v", err)
	}
	logger.Printf("CreateVolumeBackup snapshot=%s volume=%s target=%s url=%s size_bytes=%d",
		snap.UUID, snap.VolumeUUID, req.Target, bk.URL, bk.SizeBytes)
	return &weftv1.CreateVolumeBackupResponse{Backup: toVolumeBackupInfo(bk, snap.ProjectUUID)}, nil
}

// ListVolumeBackups walks the target's metadata + filters by the
// caller's visible projects. A backup's project comes from its
// origin volume — if the volume row was deleted post-backup the
// project label is the one weft stamped into the backup metadata
// at create-time (Labels["weft.project_uuid"]).
func (s *weftServer) ListVolumeBackups(ctx context.Context, req *weftv1.ListVolumeBackupsRequest) (*weftv1.ListVolumeBackupsResponse, error) {
	if req.Target == "" {
		return nil, status.Errorf(codes.InvalidArgument, "target is required")
	}
	// Project scope. Empty → caller's visible projects (we filter
	// post-list). Non-empty → resolve through AuthorizeProject so
	// an alias surfaces a clean PermissionDenied before we round-
	// trip to the target store.
	var wantProject string
	if req.Project != "" {
		projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		wantProject = projUUID
	}
	backups, err := s.adp.ListVolumeBackups(ctx, req.Target, req.VolumeUuid)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "list backups: %v", err)
	}
	out := make([]*weftv1.VolumeBackupInfo, 0, len(backups))
	for _, bk := range backups {
		project := bk.Labels["weft.project_uuid"]
		if wantProject != "" && project != wantProject {
			continue
		}
		out = append(out, toVolumeBackupInfo(bk, project))
	}
	return &weftv1.ListVolumeBackupsResponse{Backups: out}, nil
}

// DeleteVolumeBackup drops one backup from the target store. The URL
// is opaque to weft ; we forward it to the block driver, which knows
// how to strip per-target trailers (auth, scheme prefix) and emit a
// Delete on the underlying Target.
func (s *weftServer) DeleteVolumeBackup(ctx context.Context, req *weftv1.DeleteVolumeBackupRequest) (*weftv1.DeleteVolumeBackupResponse, error) {
	if req.Url == "" {
		return nil, status.Errorf(codes.InvalidArgument, "url is required")
	}
	if err := s.adp.DeleteVolumeBackup(ctx, req.Url); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete backup: %v", err)
	}
	logger.Printf("DeleteVolumeBackup url=%s", req.Url)
	return &weftv1.DeleteVolumeBackupResponse{}, nil
}

// RestoreVolumeBackup creates a fresh block-backend volume in the
// requested project + populates it from the backup. Size is
// discovered from the backup's sidecar metadata ; the operator
// doesn't have to specify it.
func (s *weftServer) RestoreVolumeBackup(ctx context.Context, req *weftv1.RestoreVolumeBackupRequest) (*weftv1.RestoreVolumeBackupResponse, error) {
	if req.Url == "" {
		return nil, status.Errorf(codes.InvalidArgument, "url is required")
	}
	if req.NewVolumeName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "new_volume_name is required")
	}
	if req.Project == "" {
		return nil, status.Errorf(codes.InvalidArgument, "project is required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	v, err := s.adp.RestoreVolumeBackup(ctx, req.Url, req.NewVolumeName, projUUID)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "restore backup: %v", err)
	}
	logger.Printf("RestoreVolumeBackup url=%s new_volume_uuid=%s name=%s project=%s",
		req.Url, v.UUID, v.Name, v.ProjectUUID)
	return &weftv1.RestoreVolumeBackupResponse{Volume: toVolumeInfo(v)}, nil
}
