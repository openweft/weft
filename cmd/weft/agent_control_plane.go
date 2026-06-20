package main

// agent_control_plane.go implements the AgentControlPlane gRPC service
// (weft-proto agentv1, defined in agent.proto). This is the
// machine-to-machine surface remote weft-agents use to register with a
// central control plane, bump their heartbeat, and surface their
// driver capabilities.
//
// Separate from the WeftAgent service (weft.proto) which is the
// operator-facing surface : same agent process serves both, but
// AgentControlPlane is gated on agent-identity auth (mTLS client
// cert in the production deploy) while WeftAgent uses OIDC.
//
// The lifecycle calls (RegisterAgent, Heartbeat) delegate to the
// existing host-registry Adapter — same code path the operator
// surface uses via RegisterHost / HeartbeatHost. The AttachDrivers
// bidi stream is wired but actual driver dispatch travels over the
// AgentDispatch service (weft.proto, already in production) ; the
// AttachDrivers stream here accepts the Init capability advertisement
// + a clean disconnect signal so future cross-cluster federation can
// route through it.

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	weft "github.com/openweft/weft"
	agentv1 "github.com/openweft/weft-proto/agentv1"
)

// agentControlPlaneServer satisfies agentv1.AgentControlPlaneServer.
// Holds a reference to the adapter so it can translate gRPC requests
// into the same registry helpers the WeftAgent surface uses.
type agentControlPlaneServer struct {
	agentv1.UnimplementedAgentControlPlaneServer
	adp weft.VZAdapter
}

// RegisterAgent translates the AgentControlPlane wire shape into the
// local RegisterHostSpec and persists it via the host registry. The
// idempotency contract matches WeftAgent.RegisterHost : empty UUID
// mints a fresh one ; non-empty UUID re-registers (refreshes mutable
// fields, preserves CreatedAt + the placement metadata when the spec
// leaves them empty).
func (s *agentControlPlaneServer) RegisterAgent(ctx context.Context, req *agentv1.RegisterAgentRequest) (*agentv1.RegisterAgentResponse, error) {
	if err := weft.RequireAdmin(ctx, "register agent"); err != nil {
		return nil, err
	}
	if req.Registration == nil {
		return nil, status.Error(codes.InvalidArgument, "registration is required")
	}
	r := req.Registration
	spec := weft.RegisterHostSpec{
		UUID:           r.Uuid,
		Hostname:       r.Hostname,
		AZ:             r.Az,
		Rack:           r.Rack,
		Endpoint:       r.Endpoint,
		Hypervisor:     r.Hypervisor,
		Architecture:   r.Architecture,
		NetworkTypes:   append([]string(nil), r.NetworkTypes...),
		VolumeBackends: append([]string(nil), r.VolumeBackends...),
		Properties:     cloneStringMap(r.Properties),
	}
	host, err := s.adp.RegisterHost(spec)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "register agent: %v", err)
	}
	return &agentv1.RegisterAgentResponse{AssignedUuid: host.UUID}, nil
}

// Heartbeat bumps the host's LastSeenAt + flips Down → Active. Same
// semantics as WeftAgent.HeartbeatHost.
func (s *agentControlPlaneServer) Heartbeat(ctx context.Context, req *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
	if err := weft.RequireAdmin(ctx, "heartbeat"); err != nil {
		return nil, err
	}
	if req.HostUuid == "" {
		return nil, status.Error(codes.InvalidArgument, "host_uuid is required")
	}
	if err := s.adp.HeartbeatHost(req.HostUuid); err != nil {
		return nil, status.Errorf(codes.NotFound, "heartbeat: %v", err)
	}
	return &agentv1.HeartbeatResponse{}, nil
}

// AttachDrivers reads the agent's Init frame (capability
// advertisement), logs the driver kinds the agent advertised, then
// drains until the stream closes. The actual driver dispatch travels
// over AgentDispatch.Connect (weft.proto, already in production) ;
// AttachDrivers exists for cross-cluster federation work where a
// dedicated dispatch path with stricter auth is desirable. Until
// that path lands, the agent's drivers stay reachable via
// AgentDispatch on the same connection.
//
// Implementation contract :
//   1. First frame MUST be Init ; anything else closes the stream
//      with InvalidArgument.
//   2. After Init, the server records the (host_uuid, driver_kinds)
//      capability advertisement so future work has a hook.
//   3. The server drains incoming frames (Dispatch / Disconnect)
//      without responding ; clients should treat AttachDrivers as
//      "accepted, dispatch via AgentDispatch" today.
func (s *agentControlPlaneServer) AttachDrivers(stream agentv1.AgentControlPlane_AttachDriversServer) error {
	if err := weft.RequireAdmin(stream.Context(), "attach drivers"); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return status.Errorf(codes.Canceled, "attach drivers: recv init: %v", err)
	}
	init, ok := first.Body.(*agentv1.AttachDriversFrame_Init)
	if !ok || init.Init == nil {
		return status.Error(codes.InvalidArgument, "first frame must be Init")
	}
	logger.Printf("AttachDrivers accepted : host=%s kinds=%v (dispatch via AgentDispatch)",
		init.Init.HostUuid, init.Init.DriverKinds)
	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return status.Errorf(codes.Canceled, "attach drivers: recv: %v", err)
		}
		if d, ok := frame.Body.(*agentv1.AttachDriversFrame_Disconnect); ok {
			logger.Printf("AttachDrivers disconnect : host=%s reason=%s",
				init.Init.HostUuid, d.Disconnect.Reason)
			return nil
		}
	}
}

