package main

// vm_status.go implements the V0.13.0 SetVMStatus gRPC RPC.
//
// Status is the operator's administrative intent, orthogonal to
// the runtime State the agent observes. Values:
//   - "active"   (default) — scheduler + respawn touch the VM freely
//   - "inactive" — frozen ; respawn skips, scheduler avoids
//   - "draining" — finish current work but don't replace on failure
//
// The AZ/Rack/Host inactive-cascade flows down to VM.Status so the
// operator's intent survives a transient runtime State flap.

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// normalisedVMStatus maps the registry's stored value to what
// clients should see on the wire : empty string → "active" so the
// TUI/webui can render a non-blank cell for legacy VMs that
// pre-date the field. Centralised here so every VMInfo emission
// site converges on the same normalisation.
func normalisedVMStatus(s string) string {
	if s == "" {
		return "active"
	}
	return s
}

func (s *weftServer) SetVMStatus(ctx context.Context, req *weftv1.SetVMStatusRequest) (*weftv1.SetVMStatusResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	// Empty status normalises to "active" downstream ; reject
	// unknown values up front so the caller sees a clean
	// InvalidArgument instead of a generic Internal.
	switch req.Status {
	case "", "active", "inactive", "draining":
		// valid
	default:
		return nil, status.Errorf(codes.InvalidArgument, "status must be active|inactive|draining (got %q)", req.Status)
	}
	if err := weft.RequireAdmin(ctx, "set vm status"); err != nil {
		return nil, err
	}
	if err := s.adp.SetVMStatus(req.Uuid, req.Status); err != nil {
		return nil, status.Errorf(codes.NotFound, "set vm status: %v", err)
	}
	v, ok := s.adp.VMByUUID(req.Uuid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "vm %q not found", req.Uuid)
	}
	return &weftv1.SetVMStatusResponse{
		Vm: &weftv1.VMInfo{
			Name:        v.Name,
			Uuid:        v.UUID,
			ProjectUuid: v.ProjectUUID,
			HostUuid:    v.HostUUID,
			Status:      v.Status,
		},
	}, nil
}
