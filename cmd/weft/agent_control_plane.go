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
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	weft "github.com/openweft/weft"
	agentv1 "github.com/openweft/weft-proto/agentv1"
)

// attachDriversCalls is the package-level Prometheus counter for the
// AttachDrivers bidi stream. Labelled by `result` :
//
//	opened       — first Init frame accepted (one tick per stream that
//	               passed auth + validated Init shape)
//	init_error   — first frame was missing/non-Init OR recv'ing it
//	               failed with a non-EOF transport error
//	client_eof   — the client closed its half cleanly (the expected
//	               steady-state termination for short-lived sessions)
//	server_eof   — the server returned nil from a non-EOF condition
//	               (reserved for future Disconnect-initiated tear-down)
//	error        — recv loop returned a non-EOF transport error
//
// Note : in the normal flow a single stream contributes TWO samples
// — one `opened` at Init-acceptance and one of {client_eof,
// server_eof, error} at termination. Operators sum on the latter
// three to count session terminations.
//
// Constructed via newAttachDriversMetrics so tests can register it
// against an isolated *prometheus.Registry without colliding with
// the agent's process-wide registry.
var attachDriversCalls *prometheus.CounterVec

// newAttachDriversMetrics builds the counter + registers it against
// `reg`. Returns the *CounterVec for direct assertion in tests. The
// agent boot path in main.go calls this once with the shared
// registry ; passing nil registers nothing and leaves the package-
// level handle wired to a detached CounterVec so the production
// handler still increments harmlessly when metrics are disabled.
func newAttachDriversMetrics(reg prometheus.Registerer) (*prometheus.CounterVec, error) {
	cv := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "weft_attach_drivers_calls_total",
		Help: "AgentControlPlane.AttachDrivers bidi-stream samples, labelled by termination cause (opened on Init accept ; init_error / client_eof / server_eof / error at termination). Until the AgentDispatch→AttachDrivers migration lands this counter is the only signal that an agent is exercising the new path in the wild.",
	}, []string{"result"})
	if reg != nil {
		if err := reg.Register(cv); err != nil {
			if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
				existing, _ := are.ExistingCollector.(*prometheus.CounterVec)
				attachDriversCalls = existing
				return existing, nil
			}
			return nil, err
		}
	}
	attachDriversCalls = cv
	return cv, nil
}

// recordAttachResult increments the counter for the given result label
// if metrics have been installed ; otherwise a no-op so tests + boot
// paths without metrics enabled stay valid.
func recordAttachResult(result string) {
	if attachDriversCalls != nil {
		attachDriversCalls.WithLabelValues(result).Inc()
	}
}

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
// drains until the stream closes. The actual driver dispatch still
// travels over AgentDispatch.Connect (weft.proto, already in
// production) ; AttachDrivers exists for cross-cluster federation
// work where a dedicated dispatch path with stricter auth is
// desirable.
//
// v0.4.49 — observability seam : the handler now stamps a
// `weft_attach_drivers_calls_total{result=…}` Prometheus counter on
// every Init-accept + every termination, and best-effort forwards
// each inbound non-Init frame to the Adapter's EventBus as a
// `agent.attach_drivers.event` PlatformEvent. Neither side is part of
// the real dispatch path : the counter is purely so operators can see
// whether anyone is calling AttachDrivers in the wild before we
// commit to the full migration, and the bus forwarding lets existing
// subscribers (TUI tail, weft-doctor) see frames flow without a new
// proto-level wiring effort. The full migration from
// AgentDispatch.Connect → AttachDrivers as the primary driver-
// dispatch path is a separate ~800-line architectural change (call-id
// correlation, per-driver-kind payload codec, integration with the
// existing dispatchSrv registry) and is NOT in this slice.
//
// TODO(v0.5.x — federation track) :
//   - introduce a per-call-id correlation table mirroring
//     dispatchSrv.sessions so Result frames can route back to the
//     issuing caller goroutine.
//   - lift Dispatch payloads through the same multidriver fan-out
//     dispatchSrv uses today, swapping the AgentDispatch transport
//     for this bidi stream once parity is proven.
//   - retire AgentDispatch.Connect in favour of AttachDrivers and
//     delete the legacy session-down hook in main.go.
//
// Implementation contract (unchanged) :
//   1. First frame MUST be Init ; anything else closes the stream
//      with InvalidArgument.
//   2. After Init, the server records the (host_uuid, driver_kinds)
//      capability advertisement so future work has a hook.
//   3. The server drains incoming frames (Dispatch / Result /
//      Disconnect) without responding ; clients should treat
//      AttachDrivers as "accepted, dispatch via AgentDispatch" today.
func (s *agentControlPlaneServer) AttachDrivers(stream agentv1.AgentControlPlane_AttachDriversServer) error {
	if err := weft.RequireAdmin(stream.Context(), "attach drivers"); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			// Clean close before Init — count as init_error so it
			// stands out separately from a no-frame-at-all stall.
			recordAttachResult("init_error")
			return nil
		}
		recordAttachResult("init_error")
		return status.Errorf(codes.Canceled, "attach drivers: recv init: %v", err)
	}
	init, ok := first.Body.(*agentv1.AttachDriversFrame_Init)
	if !ok || init.Init == nil {
		recordAttachResult("init_error")
		return status.Error(codes.InvalidArgument, "first frame must be Init")
	}
	recordAttachResult("opened")
	hostUUID := init.Init.HostUuid
	logger.Printf("AttachDrivers accepted : host=%s kinds=%v (dispatch via AgentDispatch)",
		hostUUID, init.Init.DriverKinds)
	// Best-effort PlatformEvent for the Init capability advertisement
	// itself, so operators watching the bus see something on the
	// first stream a host opens. Kind is "agent.attach_drivers.event"
	// per the v0.4.49 forwarding contract ; raw_kind labels the
	// oneof case so subscribers can fan-out later without a schema bump.
	s.forwardFrameEvent(hostUUID, "init", nil)
	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				recordAttachResult("client_eof")
				return nil
			}
			recordAttachResult("error")
			return status.Errorf(codes.Canceled, "attach drivers: recv: %v", err)
		}
		switch body := frame.Body.(type) {
		case *agentv1.AttachDriversFrame_Init:
			// Duplicate Init mid-stream : tolerated today (idempotent
			// re-advertisement) ; we forward it so subscribers see
			// the capability set bump.
			s.forwardFrameEvent(hostUUID, "init", nil)
		case *agentv1.AttachDriversFrame_Dispatch:
			s.forwardFrameEvent(hostUUID, "dispatch", nil)
		case *agentv1.AttachDriversFrame_Result:
			s.forwardFrameEvent(hostUUID, "result", nil)
		case *agentv1.AttachDriversFrame_Disconnect:
			reason := ""
			if body.Disconnect != nil {
				reason = body.Disconnect.Reason
			}
			s.forwardFrameEvent(hostUUID, "disconnect", map[string]string{"reason": reason})
			logger.Printf("AttachDrivers disconnect : host=%s reason=%s", hostUUID, reason)
			recordAttachResult("server_eof")
			return nil
		}
	}
}

// forwardFrameEvent best-effort publishes one PlatformEvent on the
// adapter's bus. Pure observability — failures (nil bus, drop) are
// swallowed so an unsubscribed event source never blocks the recv
// loop. Subject is the agent's host UUID so existing per-host bus
// filters (e.g. weft events --host) pick it up without grammar
// changes.
func (s *agentControlPlaneServer) forwardFrameEvent(hostUUID, rawKind string, extra map[string]string) {
	if s.adp == nil {
		return
	}
	bus := s.adp.EventBus()
	if bus == nil {
		return
	}
	meta := map[string]string{"raw_kind": rawKind}
	for k, v := range extra {
		meta[k] = v
	}
	defer func() {
		// Bus.Publish is non-blocking by contract but a misconfigured
		// NATSEventBus can panic on a closed conn ; the recv loop must
		// not die because telemetry forwarding misbehaved.
		if r := recover(); r != nil {
			slog.Default().Warn("attach_drivers: bus publish panic recovered", "host", hostUUID, "raw_kind", rawKind, "recover", r)
		}
	}()
	bus.Publish(weft.PlatformEvent{
		TsUnixNano: time.Now().UnixNano(),
		Kind:       "agent.attach_drivers.event",
		Subject:    hostUUID,
		Meta:       meta,
	})
}

