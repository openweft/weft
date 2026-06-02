package main

// floating_ips.go implements the five FloatingIP RPCs. Pattern
// matches security_groups.go : auth via AuthorizeProject, call into
// the adapter, marshal the persisted entry back into the proto
// shape. The adapter itself publishes "floating_ip.*" platform
// events on every mutation — handlers don't have to re-emit.

import (
	"context"
	"time"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func toFloatingIPInfo(f weft.FloatingIP) *weftv1.FloatingIPInfo {
	return &weftv1.FloatingIPInfo{
		Uuid:              f.UUID,
		Address:           f.Address,
		Network:           f.NetworkUUID,
		ProjectUuid:       f.ProjectUUID,
		MappedTo:          f.MappedTo,
		Status:            string(f.Status),
		AllocatedAtUnixNs: f.AllocatedAt.UnixNano(),
	}
}

func (s *weftServer) ListFloatingIPs(ctx context.Context, req *weftv1.ListFloatingIPsRequest) (*weftv1.ListFloatingIPsResponse, error) {
	_, all, err := s.adp.VisibleProjects(ctx)
	if err != nil {
		return nil, err
	}
	var fips []weft.FloatingIP
	if all {
		fips = s.adp.ListFloatingIPs()
	} else {
		visible, _, _ := s.adp.VisibleProjects(ctx)
		for projUUID := range visible {
			fips = append(fips, s.adp.ListFloatingIPsForProject(projUUID)...)
		}
	}
	out := make([]*weftv1.FloatingIPInfo, len(fips))
	for i, f := range fips {
		out[i] = toFloatingIPInfo(f)
	}
	return &weftv1.ListFloatingIPsResponse{FloatingIps: out}, nil
}

func (s *weftServer) AllocateFloatingIP(ctx context.Context, req *weftv1.AllocateFloatingIPRequest) (*weftv1.AllocateFloatingIPResponse, error) {
	if req.Network == "" {
		return nil, status.Error(codes.InvalidArgument, "network is required")
	}
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	networkUUID, err := resolveNetworkRef(s.adp, projUUID, req.Network)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "network %q: %v", req.Network, err)
	}
	fip, err := s.adp.AllocateFloatingIP(projUUID, networkUUID, "")
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "allocate floating ip: %v", err)
	}
	logger.Printf("AllocateFloatingIP project=%s network=%s uuid=%s address=%s",
		projUUID, networkUUID, fip.UUID, fip.Address)
	return &weftv1.AllocateFloatingIPResponse{FloatingIp: toFloatingIPInfo(fip)}, nil
}

func (s *weftServer) ReleaseFloatingIP(ctx context.Context, req *weftv1.ReleaseFloatingIPRequest) (*weftv1.ReleaseFloatingIPResponse, error) {
	if _, err := s.authFloatingIP(ctx, req.Uuid); err != nil {
		return nil, err
	}
	if err := s.adp.ReleaseFloatingIP(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "release floating ip: %v", err)
	}
	logger.Printf("ReleaseFloatingIP uuid=%s", req.Uuid)
	return &weftv1.ReleaseFloatingIPResponse{}, nil
}

func (s *weftServer) MapFloatingIP(ctx context.Context, req *weftv1.MapFloatingIPRequest) (*weftv1.MapFloatingIPResponse, error) {
	if _, err := s.authFloatingIP(ctx, req.Uuid); err != nil {
		return nil, err
	}
	kind := weft.FloatingIPTargetKind(req.TargetKind)
	if kind == "" {
		kind = weft.FIPTargetVM
	}
	fip, err := s.adp.MapFloatingIP(req.Uuid, kind, req.TargetName)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "map floating ip: %v", err)
	}
	logger.Printf("MapFloatingIP uuid=%s target=%s/%s", req.Uuid, kind, req.TargetName)
	return &weftv1.MapFloatingIPResponse{FloatingIp: toFloatingIPInfo(fip)}, nil
}

func (s *weftServer) UnmapFloatingIP(ctx context.Context, req *weftv1.UnmapFloatingIPRequest) (*weftv1.UnmapFloatingIPResponse, error) {
	if _, err := s.authFloatingIP(ctx, req.Uuid); err != nil {
		return nil, err
	}
	fip, err := s.adp.UnmapFloatingIP(req.Uuid)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "unmap floating ip: %v", err)
	}
	logger.Printf("UnmapFloatingIP uuid=%s", req.Uuid)
	return &weftv1.UnmapFloatingIPResponse{FloatingIp: toFloatingIPInfo(fip)}, nil
}

// authFloatingIP is the equivalent of authSecurityGroup —
// resolves the FIP, then asks AuthorizeProject for its owning
// project so the caller's RBAC scope decides visibility.
func (s *weftServer) authFloatingIP(ctx context.Context, uuid string) (weft.FloatingIP, error) {
	if uuid == "" {
		return weft.FloatingIP{}, status.Error(codes.InvalidArgument, "uuid is required")
	}
	f, ok := s.adp.FloatingIPByUUID(uuid)
	if !ok {
		return weft.FloatingIP{}, status.Errorf(codes.PermissionDenied, "no access to floating ip %s", uuid)
	}
	if _, err := s.adp.AuthorizeProject(ctx, f.ProjectUUID); err != nil {
		return weft.FloatingIP{}, err
	}
	return f, nil
}

// resolveNetworkRef accepts either a network UUID or a network
// name within the project. Mirrors the resolution pattern most
// other handlers use for VM/Volume references.
func resolveNetworkRef(adp weft.VZAdapter, projectUUID, ref string) (string, error) {
	if n, ok := adp.NetworkByUUID(ref); ok {
		if n.ProjectUUID != projectUUID {
			return "", _floatingIPErr("network %q belongs to a different project", ref)
		}
		return n.UUID, nil
	}
	if n, ok := adp.NetworkByName(projectUUID, ref); ok {
		return n.UUID, nil
	}
	return "", _floatingIPErr("network %q not found in project", ref)
}

// _floatingIPErr is a tiny error helper kept here so the rest of
// the file stays declarative. Avoids dragging in fmt just for one
// Errorf.
func _floatingIPErr(format string, args ...any) error {
	return status.Errorf(codes.InvalidArgument, format, args...)
}

// timeNow is a seam for tests that pin the AllocatedAt stamp.
// Unused at the handler layer (the adapter stamps it) — kept here
// in case a future test wants to override.
var timeNow = time.Now
