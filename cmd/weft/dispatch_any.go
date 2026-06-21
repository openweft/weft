package main

// dispatch_any.go is the v0.4.52 cutover layer : a single
// dispatchAny() helper that picks between the two coexisting
// dispatch transports (AgentDispatch.Connect — the long-standing
// path — and AgentControlPlane.AttachDrivers — the v0.4.50 path)
// based on which one has a live session for the target host.
//
// Decision policy :
//
//   1. If the AttachDrivers registry has a session for `hostUUID`,
//      route through it. Encode the legacy DriverRequest into an
//      opaque DriverDispatchCall.Payload (proto-marshal of the
//      DriverRequest itself, driver_kind="weft", method_name=
//      "DriverRequest"). On reply, decode the DriverDispatchResult.
//      Payload back into a DriverReply.
//   2. Otherwise fall back to AgentDispatch.Connect (no transport
//      change for hosts that still open the legacy stream).
//
// This is a SERVER-SIDE switch. Until the corresponding agent
// client (cmd/weft/run_client.go's run_attach_drivers_client) is
// shipped + operators flip the URL flag, no agent opens an
// AttachDrivers session, dispatchAny.0 is never taken, and the
// behaviour is bit-identical to v0.4.51.
//
// The opaque-payload codec means the server doesn't need a per-
// method codec for the cutover : it just nests the existing
// DriverRequest envelope. The agent's AttachDrivers handler can
// either (a) recognise the "weft:DriverRequest" method and route
// to the same multidriver entry point, or (b) expose a typed per-
// method surface in a future iteration. Either way no proto change
// is required to flip the transport today.

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/openweft/weft-proto/agentv1"
	weftv1 "github.com/openweft/weft-proto"
)

// dispatchTransportLabel returns the wire-level label of the
// transport that carried (or would carry) a dispatch for the
// given host. Used in logs + by tests asserting which path took.
// "attach" — AttachDrivers session is live for the host.
// "agent_dispatch" — no AttachDrivers session ; fall back to the
//   legacy transport (may itself have no session, in which case
//   the actual Dispatch returns Unavailable).
func (s *weftServer) dispatchTransportLabel(hostUUID string) string {
	if s.attach == nil {
		return "agent_dispatch"
	}
	if _, known := lookupAttachSession(s.attach, hostUUID); known {
		return "attach"
	}
	return "agent_dispatch"
}

// dispatchAny is the central dispatch helper. Identical contract
// to (*agentDispatchServer).Dispatch — same Unavailable / Aborted /
// DeadlineExceeded shape — so call sites swap one-for-one.
func (s *weftServer) dispatchAny(ctx context.Context, hostUUID string, op *weftv1.DriverRequest) (*weftv1.DriverReply, error) {
	if op == nil {
		return nil, status.Error(codes.InvalidArgument, "nil DriverRequest")
	}
	// AttachDrivers path : only when a session exists for this host.
	if s.attach != nil {
		if _, known := lookupAttachSession(s.attach, hostUUID); known {
			return s.dispatchViaAttach(ctx, hostUUID, op)
		}
	}
	// Legacy fallback. nil dispatch = no transport at all → return
	// Unavailable rather than a nil-deref.
	if s.dispatch == nil {
		return nil, status.Errorf(codes.Unavailable, "no dispatch transport for host %s", hostUUID)
	}
	return s.dispatch.Dispatch(ctx, hostUUID, op)
}

// dispatchViaAttach encodes the DriverRequest into an opaque
// DriverDispatchCall.Payload, sends via attachSrv.Dispatch, and
// decodes the result. Errors at the encode/decode boundary become
// codes.Internal (we know the proto is correct ; if marshal fails
// something's catastrophically wrong about the runtime).
func (s *weftServer) dispatchViaAttach(ctx context.Context, hostUUID string, op *weftv1.DriverRequest) (*weftv1.DriverReply, error) {
	payload, err := proto.Marshal(op)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal DriverRequest: %v", err)
	}
	call := &agentv1.DriverDispatchCall{
		DriverKind: "weft",
		MethodName: "DriverRequest",
		Payload:    payload,
	}
	result, err := s.attach.Dispatch(ctx, hostUUID, call)
	if err != nil {
		return nil, err
	}
	// ErrorMessage on the result surface is the agent's typed error
	// (e.g. "no such VM"). Pass it through verbatim in DriverReply.
	// — that's where dispatchStartVM/StopVM/DeleteVM expect it.
	reply := &weftv1.DriverReply{}
	if len(result.Payload) > 0 {
		if err := proto.Unmarshal(result.Payload, reply); err != nil {
			return nil, status.Errorf(codes.Internal, "unmarshal DriverReply: %v", err)
		}
	}
	if result.ErrorMessage != "" && reply.Error == "" {
		reply.Error = result.ErrorMessage
	}
	return reply, nil
}

// lookupAttachSession is a small wrapper around attach.AttachSessionCount
// + a host UUID match. The server's session map isn't exported by
// name so we use the existing AttachConnectedHostUUIDs accessor.
// Linear scan is fine — operator-scale clusters have at most ~thousands
// of agents, and dispatch isn't a hot path.
func lookupAttachSession(s *agentControlPlaneServer, hostUUID string) (bool, bool) {
	for _, u := range s.AttachConnectedHostUUIDs() {
		if u == hostUUID {
			return true, true
		}
	}
	return false, false
}
