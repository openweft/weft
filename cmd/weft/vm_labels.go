package main

// vm_labels.go implements the V0.1.8 SetVMLabels gRPC RPC. Labels
// drive SchedulingRule label-based selectors + reserved-key system
// gates (`deployment.type=ci|ha` etc.).

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *weftServer) SetVMLabels(ctx context.Context, req *weftv1.SetVMLabelsRequest) (*weftv1.SetVMLabelsResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	// Project-scoped admin check : either admin globally OR member
	// of the project (same RBAC seam as the V0.1 mutators).
	if err := weft.RequireAdmin(ctx, "set vm labels"); err != nil {
		return nil, err
	}
	var labels map[string]string
	if len(req.Labels) > 0 {
		labels = make(map[string]string, len(req.Labels))
		for k, v := range req.Labels {
			labels[k] = v
		}
	}
	v, err := s.adp.SetVMLabels(req.Project, req.Name, labels)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "set vm labels: %v", err)
	}
	return &weftv1.SetVMLabelsResponse{
		Vm: &weftv1.VMInfo{
			Name:        v.Name,
			Uuid:        v.UUID,
			Project:     req.Project,
			ProjectUuid: v.ProjectUUID,
			Labels:      cloneStringMap(v.Labels),
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
