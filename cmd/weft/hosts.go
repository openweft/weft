package main

// hosts.go owns the server-side Host registry RPCs. The Adapter
// surface (`Hosts() / HostByUUID / HostByHostname / RegisterHost
// / HeartbeatHost / SetHostState / SetHostLabels / DeleteHost`)
// already exists ; this file just translates between the gRPC
// wire types and the Adapter calls + applies the standard admin
// gate.
//
// Per [[vzd-placement-rules]] the registry feeds the multi-host
// scheduler. The `rack` field on Host (and on RegisterHost) is
// the sub-AZ failure domain the scheduler uses to honour
// `placement { rack = "different" }`.

import (
	"context"

	weft "github.com/openweft/weft"
	vzdv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toHostInfo converts the registry's Host value into the wire shape.
func toHostInfo(h weft.Host) *vzdv1.HostInfo {
	hi := &vzdv1.HostInfo{
		Uuid:            h.UUID,
		Hostname:        h.Hostname,
		Az:              h.AZ,
		Rack:            h.Rack,
		Endpoint:        h.Endpoint,
		Hypervisor:      h.Hypervisor,
		Architecture:    h.Architecture,
		NetworkTypes:    append([]string(nil), h.NetworkTypes...),
		VolumeBackends:  append([]string(nil), h.VolumeBackends...),
		State:           string(h.State),
		CreatedAtUnixNs: h.CreatedAt.UnixNano(),
	}
	if !h.LastSeenAt.IsZero() {
		hi.LastSeenAtUnixNs = h.LastSeenAt.UnixNano()
	}
	if len(h.Labels) > 0 {
		hi.Labels = make(map[string]string, len(h.Labels))
		for k, v := range h.Labels {
			hi.Labels[k] = v
		}
	}
	return hi
}

func (s *vzdServer) RegisterHost(ctx context.Context, req *vzdv1.RegisterHostRequest) (*vzdv1.RegisterHostResponse, error) {
	if err := weft.RequireAdmin(ctx, "register host"); err != nil {
		return nil, err
	}
	if req.Hostname == "" {
		return nil, status.Error(codes.InvalidArgument, "hostname is required")
	}
	spec := weft.RegisterHostSpec{
		UUID:           req.Uuid,
		Hostname:       req.Hostname,
		AZ:             req.Az,
		Rack:           req.Rack,
		Endpoint:       req.Endpoint,
		Hypervisor:     req.Hypervisor,
		Architecture:   req.Architecture,
		NetworkTypes:   append([]string(nil), req.NetworkTypes...),
		VolumeBackends: append([]string(nil), req.VolumeBackends...),
	}
	if len(req.Labels) > 0 {
		spec.Labels = make(map[string]string, len(req.Labels))
		for k, v := range req.Labels {
			spec.Labels[k] = v
		}
	}
	h, err := s.adp.RegisterHost(spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register host: %v", err)
	}
	return &vzdv1.RegisterHostResponse{Host: toHostInfo(h)}, nil
}

func (s *vzdServer) ListHosts(ctx context.Context, req *vzdv1.ListHostsRequest) (*vzdv1.ListHostsResponse, error) {
	if err := weft.RequireAdmin(ctx, "list hosts"); err != nil {
		return nil, err
	}
	var hosts []weft.Host
	if req.Az != "" {
		hosts = s.adp.HostsInAZ(req.Az)
	} else {
		hosts = s.adp.Hosts()
	}
	out := make([]*vzdv1.HostInfo, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, toHostInfo(h))
	}
	var connected []string
	if s.dispatch != nil {
		connected = s.dispatch.ConnectedHostUUIDs()
	}
	return &vzdv1.ListHostsResponse{Hosts: out, ConnectedHostUuids: connected}, nil
}

// GetHost accepts either UUID or hostname. The Adapter has
// separate lookup helpers ; we surface the same disambiguation
// at the RPC boundary (exactly one of the two must be set).
func (s *vzdServer) GetHost(ctx context.Context, req *vzdv1.GetHostRequest) (*vzdv1.GetHostResponse, error) {
	if err := weft.RequireAdmin(ctx, "get host"); err != nil {
		return nil, err
	}
	if (req.Uuid == "" && req.Hostname == "") || (req.Uuid != "" && req.Hostname != "") {
		return nil, status.Error(codes.InvalidArgument, "exactly one of uuid or hostname must be set")
	}
	var (
		h  weft.Host
		ok bool
	)
	if req.Uuid != "" {
		h, ok = s.adp.HostByUUID(req.Uuid)
	} else {
		h, ok = s.adp.HostByHostname(req.Hostname)
	}
	if !ok {
		return nil, status.Errorf(codes.NotFound, "host not found")
	}
	return &vzdv1.GetHostResponse{Host: toHostInfo(h)}, nil
}

// HeartbeatHost is the per-host agent's keepalive — NOT
// admin-gated, since it runs unattended from compute nodes.
// Anonymous heartbeats are accepted today ; once per-host gRPC
// auth lands (NKey credentials per [[vzd-tenant-event-access]]
// extended to agents) this gets tightened.
func (s *vzdServer) HeartbeatHost(_ context.Context, req *vzdv1.HeartbeatHostRequest) (*vzdv1.HeartbeatHostResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.adp.HeartbeatHost(req.Uuid); err != nil {
		return nil, status.Errorf(codes.Internal, "heartbeat: %v", err)
	}
	return &vzdv1.HeartbeatHostResponse{}, nil
}

func (s *vzdServer) SetHostState(ctx context.Context, req *vzdv1.SetHostStateRequest) (*vzdv1.SetHostStateResponse, error) {
	if err := weft.RequireAdmin(ctx, "set host state"); err != nil {
		return nil, err
	}
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	st := weft.HostState(req.State)
	switch st {
	case weft.HostStateActive, weft.HostStateDraining, weft.HostStateDown:
		// valid
	default:
		return nil, status.Errorf(codes.InvalidArgument, "state must be active|draining|down (got %q)", req.State)
	}
	if err := s.adp.SetHostState(req.Uuid, st); err != nil {
		return nil, status.Errorf(codes.Internal, "set host state: %v", err)
	}
	return &vzdv1.SetHostStateResponse{}, nil
}

func (s *vzdServer) SetHostLabels(ctx context.Context, req *vzdv1.SetHostLabelsRequest) (*vzdv1.SetHostLabelsResponse, error) {
	if err := weft.RequireAdmin(ctx, "set host labels"); err != nil {
		return nil, err
	}
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	var labels map[string]string
	if len(req.Labels) > 0 {
		labels = make(map[string]string, len(req.Labels))
		for k, v := range req.Labels {
			labels[k] = v
		}
	}
	if err := s.adp.SetHostLabels(req.Uuid, labels); err != nil {
		return nil, status.Errorf(codes.Internal, "set host labels: %v", err)
	}
	return &vzdv1.SetHostLabelsResponse{}, nil
}

func (s *vzdServer) DeleteHost(ctx context.Context, req *vzdv1.DeleteHostRequest) (*vzdv1.DeleteHostResponse, error) {
	if err := weft.RequireAdmin(ctx, "delete host"); err != nil {
		return nil, err
	}
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.adp.DeleteHost(req.Uuid); err != nil {
		return nil, status.Errorf(codes.Internal, "delete host: %v", err)
	}
	return &vzdv1.DeleteHostResponse{}, nil
}
