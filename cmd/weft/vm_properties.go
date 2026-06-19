package main

// vm_properties.go implements the V0.1.8 SetVMProperties gRPC RPC. Properties
// drive SchedulingRule property-based selectors + reserved-key system
// gates (`deployment.type=ci|ha` etc.).

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *weftServer) SetVMProperties(ctx context.Context, req *weftv1.SetVMPropertiesRequest) (*weftv1.SetVMPropertiesResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	// Project-scoped admin check : either admin globally OR member
	// of the project (same RBAC seam as the V0.1 mutators).
	if err := weft.RequireAdmin(ctx, "set vm properties"); err != nil {
		return nil, err
	}
	var properties map[string]string
	if len(req.Properties) > 0 {
		properties = make(map[string]string, len(req.Properties))
		for k, v := range req.Properties {
			properties[k] = v
		}
	}
	v, err := s.adp.SetVMProperties(req.Project, req.Name, properties)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "set vm properties: %v", err)
	}
	return &weftv1.SetVMPropertiesResponse{
		Vm: &weftv1.VMInfo{
			Name:        v.Name,
			Uuid:        v.UUID,
			Project:     req.Project,
			ProjectUuid: v.ProjectUUID,
			Properties:  cloneStringMap(v.Properties),
		},
	}, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
