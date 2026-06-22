package main

// hosts.go owns the server-side Host registry RPCs. The Adapter
// surface (`Hosts() / HostByUUID / HostByHostname / RegisterHost
// / HeartbeatHost / SetHostState / SetHostProperties / DeleteHost`)
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
	"time"

	weft "github.com/openweft/weft"
	"github.com/openweft/weft/etcdcoord"
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
	if len(h.Properties) > 0 {
		hi.Properties = make(map[string]string, len(h.Properties))
		for k, v := range h.Properties {
			hi.Properties[k] = v
		}
	}
	if h.AgentVersion != "" {
		hi.AgentVersion = h.AgentVersion
	}
	if len(h.DriverVersions) > 0 {
		hi.DriverVersions = make(map[string]string, len(h.DriverVersions))
		for k, v := range h.DriverVersions {
			hi.DriverVersions[k] = v
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
		AgentVersion:   req.AgentVersion,
	}
	if len(req.DriverVersions) > 0 {
		spec.DriverVersions = make(map[string]string, len(req.DriverVersions))
		for k, v := range req.DriverVersions {
			spec.DriverVersions[k] = v
		}
	}
	if len(req.Properties) > 0 {
		spec.Properties = make(map[string]string, len(req.Properties))
		for k, v := range req.Properties {
			spec.Properties[k] = v
		}
	}
	// Attestation gate (feature-flagged, default OFF). When the flag is
	// off (s.attest nil or disabled) this block is skipped entirely and
	// the path below is exactly the legacy OIDC-only flow. When ON, the
	// caller's AK Name (carried in the properties under attestAKLabel) must
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

// attestAKLabel is the reserved RegisterHostRequest.properties key carrying
// the node's TPM AK Name (the value the Admit handler keyed on) when
// attestation is enabled. Using the existing properties map avoids a proto
// change to RegisterHostRequest ; the key is namespaced under weft.attest/
// so it can't collide with an operator property. The value is the raw AK
// Name bytes rendered as the same string key the gate's admitted-set
// uses (the node sends string(akName)).
const attestAKLabel = "weft.attest/ak-name"

// attestAKNameFromReq pulls the node's AK Name out of the request properties.
// Returns nil when absent (requireAdmittedAK then denies). The property is
// removed from the spec's properties by the caller path implicitly — we read
// it here and do NOT strip it from spec.Properties, so it is also visible on
// the Host entry for diagnostics ; AKName is the authoritative field.
func attestAKNameFromReq(req *weftv1.RegisterHostRequest) []byte {
	if req == nil || len(req.Properties) == 0 {
		return nil
	}
	v, ok := req.Properties[attestAKLabel]
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
	// The local agent never opens a Connect stream to itself — it
	// talks to the Adapter directly. But it IS reachable (the
	// caller is talking to it right now), so the dashboard should
	// reflect that. Surface localHostUUID as connected if known
	// and not already in the dispatch list ; otherwise operators
	// see "connected=no" for the host they just hit, which is
	// false negative.
	if s.localHostUUID != "" {
		dup := false
		for _, u := range connected {
			if u == s.localHostUUID {
				dup = true
				break
			}
		}
		if !dup {
			connected = append(connected, s.localHostUUID)
		}
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

func (s *weftServer) SetHostProperties(ctx context.Context, req *weftv1.SetHostPropertiesRequest) (*weftv1.SetHostPropertiesResponse, error) {
	if err := weft.RequireAdmin(ctx, "set host properties"); err != nil {
		return nil, err
	}
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	var properties map[string]string
	if len(req.Properties) > 0 {
		properties = make(map[string]string, len(req.Properties))
		for k, v := range req.Properties {
			properties[k] = v
		}
	}
	if err := s.adp.SetHostProperties(req.Uuid, properties); err != nil {
		return nil, status.Errorf(codes.Internal, "set host properties: %v", err)
	}
	return &weftv1.SetHostPropertiesResponse{}, nil
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
	// Evict the etcd liveness key so the HostWatcher sees a clean
	// HostDown event + so the orphaned lease doesn't keep
	// /weft/coord/hosts/<uuid> visible to other agents. Best-effort :
	// a failure here leaves the lease intact (it expires within
	// LeaseTTLSec=10s after the agent stops anyway) but the registry
	// delete already committed, so we don't fail the RPC.
	if s.etcdCli != nil {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := s.etcdCli.Delete(dctx, etcdcoord.HostsPrefix+req.Uuid); err != nil {
			logger.Printf("DeleteHost %s: etcd liveness eviction failed (lease will expire on its own): %v", req.Uuid, err)
		}
	}
	return &weftv1.DeleteHostResponse{}, nil
}

