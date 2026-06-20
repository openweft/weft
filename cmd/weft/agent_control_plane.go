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
	"sync"
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
	// attachSessions is the per-host AttachDrivers session registry.
	// Mirrors agentDispatchServer.sessions for the new transport ;
	// see agent_control_plane_dispatch.go for the full lifecycle.
	// Empty until the first AttachDrivers stream lands ; Dispatch()
	// returns Unavailable for hosts with no entry.
	attachMu       sync.Mutex
	attachSessions map[string]*attachDriversSession
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

// AttachDrivers is the dispatch transport. v0.4.50 promotes it from
// "Init + observability drain" to a real dispatch path with the same
// shape as the long-standing AgentDispatch.Connect : per-host session
// registry, call_id correlation, sender/receiver goroutines, and a
// public Dispatch() method on this server for other parts of the
// agent to send DriverDispatchCall frames + await their matching
// DriverDispatchResult.
//
// Co-existence : the legacy AgentDispatch.Connect transport stays
// live alongside this one. Operators choose per-deployment which
// transport their agents open (by toggling the corresponding
// control-plane URL flag on the agent client side). Migration off
// AgentDispatch is a separate decision once AttachDrivers has
// real-world burn-in.
//
// Implementation contract :
//
//  1. First frame MUST be Init ; anything else closes the stream
//     with InvalidArgument.
//  2. After Init, the server registers an attachDriversSession
//     keyed on host_uuid (supersede-by-reconnect semantics — same
//     UUID re-connecting cancels the old session's goroutines first).
//  3. Sender goroutine drains session.send → stream.Send. Receiver
//     goroutine reads Result frames and routes them through the
//     pending table to Dispatch() callers. Other frame kinds
//     (Init re-advertisement, stray Dispatch, Disconnect) take the
//     observability path (PlatformEvent forward) from v0.4.49.
//  4. Stream end (EOF, Disconnect, transport error) tears down the
//     goroutines and drains the pending table with synthetic
//     "session aborted" Results so blocked Dispatch() callers
//     unblock promptly.
//
// Observability (preserved from v0.4.49) :
//   - weft_attach_drivers_calls_total{result=…} on every Init-accept
//     + every termination.
//   - agent.attach_drivers.event PlatformEvents on every Init,
//     Disconnect, and stray Dispatch/Result frame (Result frames
//     that match a pending call go straight to the caller and don't
//     emit a bus event — would be noise).
func (s *agentControlPlaneServer) AttachDrivers(stream agentv1.AgentControlPlane_AttachDriversServer) error {
	if err := weft.RequireAdmin(stream.Context(), "attach drivers"); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
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
	logger.Printf("AttachDrivers accepted : host=%s kinds=%v",
		hostUUID, init.Init.DriverKinds)
	s.forwardFrameEvent(hostUUID, "init", nil)

	// Register the session before launching the goroutines so a
	// concurrent Dispatch() call sees it. supersede-by-reconnect :
	// if a session already exists for this host, drain its pending
	// table + cancel its context so its goroutines exit cleanly
	// before we replace it in the map.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	sess := &attachDriversSession{
		hostUUID:    hostUUID,
		stream:      stream,
		connectedAt: time.Now().UTC(),
		send:        make(chan *agentv1.AttachDriversFrame, 16),
		cancel:      cancel,
	}
	if existing := s.registerAttachSession(sess); existing != nil {
		existing.pending.drainAll("AttachDrivers session superseded by reconnect")
		if existing.cancel != nil {
			existing.cancel()
		}
	}
	defer func() {
		removed := s.deregisterAttachSession(sess)
		// Wake any Dispatch() callers blocked on a result from
		// this now-dead session.
		sess.pending.drainAll("AttachDrivers session ended")
		_ = removed // future hook : onSessionDown analog to fire host-down
	}()

	// Sender + receiver goroutines. The receiver passes a per-host
	// forwardFrame closure so non-Result frames still take the
	// PlatformEvent path from v0.4.49 — operators keep their bus
	// subscriptions intact.
	errCh := make(chan error, 2)
	forwardFrame := func(rawKind string, extra map[string]string) {
		s.forwardFrameEvent(hostUUID, rawKind, extra)
	}
	go s.runAttachSender(ctx, sess, errCh)
	go s.runAttachReceiver(sess, forwardFrame, errCh)

	err = <-errCh
	cancel()
	if err == nil {
		// Disconnect frame from the agent : observability path
		// already logged + forwarded.
		recordAttachResult("server_eof")
		return nil
	}
	if errors.Is(err, io.EOF) {
		recordAttachResult("client_eof")
		return nil
	}
	if errors.Is(err, context.Canceled) {
		// Supersede-by-reconnect cancelled us — not an error.
		recordAttachResult("server_eof")
		return nil
	}
	recordAttachResult("error")
	return status.Errorf(codes.Canceled, "attach drivers: %v", err)
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

