package main

// pod_specs_handler.go implements the SetPodSpec / GetPodSpec WeftAgent
// RPCs added in weft-proto v0.15.0. They give operators a wire-surface
// to publish a guestv1.PodSpec into the adapter's in-memory registry ;
// GuestPodPlane.Attach (guest_pod_plane.go) reads through to populate
// the HelloAck served on the vsock stream.
//
// The proto carries the spec as a protojson-encoded byte blob so
// weft.proto doesn't have to import guestv1 ; we decode here, then
// hand the typed message to Adapter.SetPodSpec which both updates
// the in-memory map and persists it to <stateDir>/podspecs.hcl
// (see pod_specs.go in the weft package).

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// SetPodSpec publishes the operator's desired PodSpec for pod_id.
// An empty / JSON-null body evicts the entry — same semantics as
// Adapter.SetPodSpec(nil). Admin-gated (the spec drives in-guest
// command execution).
func (s *weftServer) SetPodSpec(ctx context.Context, req *weftv1.SetPodSpecRequest) (*weftv1.SetPodSpecResponse, error) {
	if err := weft.RequireAdmin(ctx, "set pod spec"); err != nil {
		return nil, err
	}
	if req.PodId == "" {
		return nil, status.Error(codes.InvalidArgument, "pod_id required")
	}
	if len(req.SpecJson) == 0 || string(req.SpecJson) == "null" {
		s.adp.SetPodSpec(req.PodId, nil)
		return &weftv1.SetPodSpecResponse{}, nil
	}
	spec := &guestv1.PodSpec{}
	// DiscardUnknown so a webui built against an older proto schema
	// can still publish a spec that includes new fields the agent
	// doesn't recognise yet — forward-compat for the wire format.
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(req.SpecJson, spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec_json: protojson decode: %v", err)
	}
	s.adp.SetPodSpec(req.PodId, spec)
	logger.Printf("SetPodSpec pod=%s containers=%d", req.PodId, len(spec.Containers))
	return &weftv1.SetPodSpecResponse{}, nil
}

// GetPodSpec returns the currently-published PodSpec for pod_id, or
// found=false when no spec has been published. The spec is encoded
// back to protojson so the wire shape mirrors SetPodSpec.
func (s *weftServer) GetPodSpec(ctx context.Context, req *weftv1.GetPodSpecRequest) (*weftv1.GetPodSpecResponse, error) {
	if err := weft.RequireAdmin(ctx, "get pod spec"); err != nil {
		return nil, err
	}
	if req.PodId == "" {
		return nil, status.Error(codes.InvalidArgument, "pod_id required")
	}
	spec, ok := s.adp.PodSpec(req.PodId)
	if !ok || spec == nil {
		return &weftv1.GetPodSpecResponse{PodId: req.PodId, Found: false}, nil
	}
	raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "spec_json: protojson encode: %v", err)
	}
	return &weftv1.GetPodSpecResponse{
		PodId:    req.PodId,
		SpecJson: raw,
		Found:    true,
	}, nil
}

// GetMicroVMMetrics returns the latest per-VM metrology snapshot.
// Today the agent doesn't yet sample from the hypervisor driver
// (Apple-VZ statistics surface, QEMU QMP query-blockstats, …), so
// every field is zero with sampled_at_unix_ns = 0. The webui Metrics
// tab now renders this empty shape natively instead of falling back
// to synthetic mock data via wclient's IsUnimplemented path. Real
// telemetry wiring is a follow-up that updates this handler only.
func (s *weftServer) GetMicroVMMetrics(ctx context.Context, req *weftv1.GetMicroVMMetricsRequest) (*weftv1.MicroVMMetricsResponse, error) {
	// Light auth gate : projects-the-caller-can-see scopes the lookup ;
	// the metrics surface doesn't reveal secrets but we still don't want
	// arbitrary callers probing UUID space. AuthorizeProject returns
	// the resolved UUID (or "" when req.Project is empty / dev mode).
	projUUID, err := s.adp.AuthorizeProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	vmUUID := req.VmUuid
	if vmUUID == "" {
		// Resolve (name, project) → UUID via the local registry. The
		// scoping (project filter) mirrors VMStatus above.
		if req.Name == "" {
			return nil, status.Error(codes.InvalidArgument, "vm_uuid or name required")
		}
		if rec, ok := s.adp.VMByName(projUUID, req.Name); ok {
			vmUUID = rec.UUID
		}
	}
	// Per-VM runtime telemetry isn't yet wired to the hypervisor driver
	// surfaces. Return the zero shape with vm_uuid populated so callers
	// can correlate ; sampled_at_unix_ns = 0 signals "no sample taken
	// yet" cleanly (vs. wclient's previous Unimplemented fallback).
	return &weftv1.MicroVMMetricsResponse{
		VmUuid:           vmUUID,
		SampledAtUnixNs:  0,
		CpuPercent:       0,
		MemUsedMib:       0,
		MemTotalMib:      0,
		NetRxBps:         0,
		NetTxBps:         0,
		DiskReadBps:      0,
		DiskWriteBps:     0,
		UptimeMs:         0,
	}, nil
}
