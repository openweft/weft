package main

// hosts.go owns the server-side Host registry RPCs. The Adapter
// surface (`Hosts() / HostByUUID / HostByHostname / RegisterHost
// / HeartbeatHost / SetHostState / SetHostLabels / DeleteHost`)
// already exists ; this file just translates between the gRPC
// wire types and the Adapter calls + applies the standard admin
// gate.
//
// Per [[weft-placement-rules]] the registry feeds the multi-host
// scheduler. The `rack` field on Host (and on RegisterHost) is
// the sub-AZ failure domain the scheduler uses to honour
// `placement { rack = "different" }`.

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// toHostInfo converts the registry's Host value into the wire shape.
func toHostInfo(h weft.Host) *weftv1.HostInfo {
	hi := &weftv1.HostInfo{
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
		Cordoned:        h.Cordoned,
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

func (s *weftServer) RegisterHost(ctx context.Context, req *weftv1.RegisterHostRequest) (*weftv1.RegisterHostResponse, error) {
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
	// Attestation gate (feature-flagged, default OFF). When the flag is
	// off (s.attest nil or disabled) this block is skipped entirely and
	// the path below is exactly the legacy OIDC-only flow. When ON, the
	// caller's AK Name (carried in the labels under attestAKLabel) must
	// have a fresh successful Admit, and the admitted AK Name is stamped
	// onto the Host registry entry.
	if s.attestGateEnabled() {
		akName := attestAKNameFromReq(req)
		if err := s.requireAdmittedAK(akName); err != nil {
			return nil, err
		}
		spec.AKName = string(akName)
	}
	h, err := s.adp.RegisterHost(spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register host: %v", err)
	}
	return &weftv1.RegisterHostResponse{Host: toHostInfo(h)}, nil
}

// attestAKLabel is the reserved RegisterHostRequest.labels key carrying
// the node's TPM AK Name (the value the Admit handler keyed on) when
// attestation is enabled. Using the existing labels map avoids a proto
// change to RegisterHostRequest ; the key is namespaced under weft.attest/
// so it can't collide with an operator label. The value is the raw AK
// Name bytes rendered as the same string key the gate's admitted-set
// uses (the node sends string(akName)).
const attestAKLabel = "weft.attest/ak-name"

// attestAKNameFromReq pulls the node's AK Name out of the request labels.
// Returns nil when absent (requireAdmittedAK then denies). The label is
// removed from the spec's labels by the caller path implicitly — we read
// it here and do NOT strip it from spec.Labels, so it is also visible on
// the Host entry for diagnostics ; AKName is the authoritative field.
func attestAKNameFromReq(req *weftv1.RegisterHostRequest) []byte {
	if req == nil || len(req.Labels) == 0 {
		return nil
	}
	v, ok := req.Labels[attestAKLabel]
	if !ok || v == "" {
		return nil
	}
	return []byte(v)
}

func (s *weftServer) ListHosts(ctx context.Context, req *weftv1.ListHostsRequest) (*weftv1.ListHostsResponse, error) {
	if err := weft.RequireAdmin(ctx, "list hosts"); err != nil {
		return nil, err
	}
	var hosts []weft.Host
	if req.Az != "" {
		hosts = s.adp.HostsInAZ(req.Az)
	} else {
		hosts = s.adp.Hosts()
	}
	out := make([]*weftv1.HostInfo, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, toHostInfo(h))
	}
	var connected []string
	if s.dispatch != nil {
		connected = s.dispatch.ConnectedHostUUIDs()
	}
	return &weftv1.ListHostsResponse{Hosts: out, ConnectedHostUuids: connected}, nil
}

// GetHost accepts either UUID or hostname. The Adapter has
// separate lookup helpers ; we surface the same disambiguation
// at the RPC boundary (exactly one of the two must be set).
func (s *weftServer) GetHost(ctx context.Context, req *weftv1.GetHostRequest) (*weftv1.GetHostResponse, error) {
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
	return &weftv1.GetHostResponse{Host: toHostInfo(h)}, nil
}

// HeartbeatHost is the per-host agent's keepalive — NOT
// admin-gated, since it runs unattended from compute nodes.
// Anonymous heartbeats are accepted today ; once per-host gRPC
// auth lands (NKey credentials per [[weft-tenant-event-access]]
// extended to agents) this gets tightened.
func (s *weftServer) HeartbeatHost(_ context.Context, req *weftv1.HeartbeatHostRequest) (*weftv1.HeartbeatHostResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.adp.HeartbeatHost(req.Uuid); err != nil {
		return nil, status.Errorf(codes.Internal, "heartbeat: %v", err)
	}
	return &weftv1.HeartbeatHostResponse{}, nil
}

func (s *weftServer) SetHostState(ctx context.Context, req *weftv1.SetHostStateRequest) (*weftv1.SetHostStateResponse, error) {
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
	return &weftv1.SetHostStateResponse{}, nil
}

func (s *weftServer) SetHostLabels(ctx context.Context, req *weftv1.SetHostLabelsRequest) (*weftv1.SetHostLabelsResponse, error) {
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
	return &weftv1.SetHostLabelsResponse{}, nil
}

// SetHostCordoned toggles the per-host cordon flag. Idempotent —
// calling with the current value returns OK without publishing an
// event. Admin-gated like every other cluster-scoped host verb ;
// the agent-driven heartbeat path stays untouched.
func (s *weftServer) SetHostCordoned(ctx context.Context, req *weftv1.SetHostCordonedRequest) (*weftv1.SetHostCordonedResponse, error) {
	if err := weft.RequireAdmin(ctx, "set host cordoned"); err != nil {
		return nil, err
	}
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.adp.SetHostCordoned(req.Uuid, req.Cordoned); err != nil {
		return nil, status.Errorf(codes.Internal, "set host cordoned: %v", err)
	}
	return &weftv1.SetHostCordonedResponse{}, nil
}

func (s *weftServer) DeleteHost(ctx context.Context, req *weftv1.DeleteHostRequest) (*weftv1.DeleteHostResponse, error) {
	if err := weft.RequireAdmin(ctx, "delete host"); err != nil {
		return nil, err
	}
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := s.adp.DeleteHost(req.Uuid); err != nil {
		return nil, status.Errorf(codes.Internal, "delete host: %v", err)
	}
	return &weftv1.DeleteHostResponse{}, nil
}
