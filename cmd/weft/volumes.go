package main

// volumes.go implements the seven Volume RPCs on top of the
// Adapter's volume registry wrappers (pkg/openweft/weft/volumes.go
// + the Adapter delegations in adapter.go). All mutation RPCs
// flow through the ACL layer:
//
//   * ListVolumes — scoped to caller's VisibleProjects (or one
//     explicit project the caller is authorised on).
//   * CreateVolume — caller must have access to the target
//     project (AuthorizeProject).
//   * Rename/Resize/Attach/Detach/Delete — caller must have
//     access to the volume's owning project (looked up via the
//     UUID before the mutation).
//
// Same authorisation discipline as projects + VMs: never trust
// the request; always resolve through the Adapter helpers in
// acl.go.

import (
	"context"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func toVolumeInfo(v weft.Volume) *weftv1.VolumeInfo {
	return &weftv1.VolumeInfo{
		Uuid:            v.UUID,
		ProjectUuid:     v.ProjectUUID,
		Name:            v.Name,
		SizeGib:         int64(v.SizeGiB),
		Format:          string(v.Format),
		AttachedToUuid:  v.AttachedTo,
		CreatedAtUnixNs: v.CreatedAt.UnixNano(),
	}
}

func (s *weftServer) ListVolumes(ctx context.Context, req *weftv1.ListVolumesRequest) (*weftv1.ListVolumesResponse, error) {
	visible, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	// Optional project narrowing: resolve to UUID first so the
	// caller can use either a display name or a UUID.
	var wantProjectUUID string
	if req.Project != "" {
		uuid, err := s.adp.AuthorizeProject(ctx, req.Project)
		if err != nil {
			return nil, err
		}
		wantProjectUUID = uuid
	}
	out := []*weftv1.VolumeInfo{}
	for _, v := range s.adp.Volumes() {
		if wantProjectUUID != "" && v.ProjectUUID != wantProjectUUID {
			continue
		}
		if !all {
			if _, ok := visible[v.ProjectUUID]; !ok {
				continue
			}
		}
		out = append(out, toVolumeInfo(v))
	}
	return &weftv1.ListVolumesResponse{Volumes: out}, nil
}

func (s *weftServer) CreateVolume(ctx context.Context, req *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if req.SizeGib <= 0 {
		return nil, status.Error(codes.InvalidArgument, "size_gib must be > 0")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	// Per-project volume_gib cap. Zero cap = unlimited (the
	// quota helper short-circuits). ResourceExhausted on breach
	// — clients translate to "quota exceeded" without needing
	// handler-specific awareness. See
	// docs/operations/tenant-quotas.md.
	if err := s.adp.EnforceTenantQuotaForVolume(projUUID, int(req.SizeGib)); err != nil {
		return nil, err
	}
	format := weft.VolumeFormat(req.Format)
	if format == "" {
		format = weft.VolumeFormatRaw
	}
	v, err := s.adp.CreateVolume(weft.CreateVolumeSpec{
		ProjectUUID: projUUID,
		Name:        req.Name,
		SizeGiB:     int(req.SizeGib),
		Format:      format,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create volume: %v", err)
	}
	logger.Printf("CreateVolume name=%s project=%s uuid=%s size_gib=%d", v.Name, v.ProjectUUID, v.UUID, v.SizeGiB)
	return &weftv1.CreateVolumeResponse{Volume: toVolumeInfo(v)}, nil
}

// authVolume looks up a volume by UUID and runs the caller through
// the project-ACL gate. Returns the resolved Volume so the handler
// can chain the actual mutation without a second lookup.
func (s *weftServer) authVolume(ctx context.Context, uuid string) (weft.Volume, error) {
	if uuid == "" {
		return weft.Volume{}, status.Error(codes.InvalidArgument, "uuid is required")
	}
	v, ok := s.adp.VolumeByUUID(uuid)
	if !ok {
		// Don't leak existence: caller may not be allowed to see
		// this project. Return PermissionDenied so the two cases
		// look identical from outside.
		return weft.Volume{}, status.Errorf(codes.PermissionDenied, "no access to volume %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, v.ProjectUUID); err != nil {
		return weft.Volume{}, err
	}
	return v, nil
}

func (s *weftServer) RenameVolume(ctx context.Context, req *weftv1.RenameVolumeRequest) (*weftv1.RenameVolumeResponse, error) {
	if req.NewName == "" {
		return nil, status.Error(codes.InvalidArgument, "new_name is required")
	}
	if _, err := s.authVolume(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.RenameVolume(req.Uuid, req.NewName); err != nil {
		return nil, status.Errorf(codes.Internal, "rename volume: %v", err)
	}
	v, _ := s.adp.VolumeByUUID(req.Uuid)
	return &weftv1.RenameVolumeResponse{Volume: toVolumeInfo(v)}, nil
}

func (s *weftServer) ResizeVolume(ctx context.Context, req *weftv1.ResizeVolumeRequest) (*weftv1.ResizeVolumeResponse, error) {
	if req.NewSizeGib <= 0 {
		return nil, status.Error(codes.InvalidArgument, "new_size_gib must be > 0")
	}
	if _, err := s.authVolume(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.ResizeVolume(req.Uuid, int(req.NewSizeGib)); err != nil {
		return nil, status.Errorf(codes.Internal, "resize volume: %v", err)
	}
	v, _ := s.adp.VolumeByUUID(req.Uuid)
	return &weftv1.ResizeVolumeResponse{Volume: toVolumeInfo(v)}, nil
}

func (s *weftServer) AttachVolume(ctx context.Context, req *weftv1.AttachVolumeRequest) (*weftv1.AttachVolumeResponse, error) {
	if req.VmUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_uuid is required")
	}
	if _, err := s.authVolume(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.AttachVolume(req.Uuid, req.VmUuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "attach volume: %v", err)
	}
	v, _ := s.adp.VolumeByUUID(req.Uuid)
	return &weftv1.AttachVolumeResponse{Volume: toVolumeInfo(v)}, nil
}

func (s *weftServer) DetachVolume(ctx context.Context, req *weftv1.DetachVolumeRequest) (*weftv1.DetachVolumeResponse, error) {
	if _, err := s.authVolume(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DetachVolume(req.Uuid); err != nil {
		return nil, status.Errorf(codes.Internal, "detach volume: %v", err)
	}
	v, _ := s.adp.VolumeByUUID(req.Uuid)
	return &weftv1.DetachVolumeResponse{Volume: toVolumeInfo(v)}, nil
}

func (s *weftServer) DeleteVolume(ctx context.Context, req *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error) {
	if _, err := s.authVolume(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteVolume(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete volume: %v", err)
	}
	logger.Printf("DeleteVolume uuid=%s", req.Uuid)
	return &weftv1.DeleteVolumeResponse{}, nil
}
