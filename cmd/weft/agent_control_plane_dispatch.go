package main

// agent_control_plane_dispatch.go owns the dispatch half of
// AgentControlPlane.AttachDrivers : per-stream session registry,
// pending call-id correlation, sender/receiver goroutines, and the
// public Dispatch() method the rest of the agent calls when it
// wants to route an op over the AttachDrivers transport.
//
// Mirrors agent_dispatch.go (the AgentDispatch.Connect transport)
// in shape : one session per connected agent, a buffered send
// channel funnelling outbound frames, a pending map keying call_id
// → reply chan, drainAll on disconnect. Differences :
//
//   * AttachDrivers carries opaque per-method bytes
//     (DriverDispatchCall.Payload) — no equivalent to AgentDispatch's
//     ControlMessage/AgentMessage envelope multiplexing. The codec
//     belongs to the caller (marshalling per-method-proto into bytes
//     ; the agent reverses it). This file is transport-only.
//
//   * call_id is a uint64 the server mints + threads through the
//     pending table ; the agent echoes it on the matching Result.
//     uint64 is enough for trillions of calls per stream — no
//     wraparound risk before a session ends.
//
//   * No keepalive / liveness loop here. Yet. The Init frame is the
//     only handshake ; the underlying TCP/Unix-socket transport
//     surfaces dead connections as Recv errors which terminate the
//     session naturally. If long-idle streams turn out to be a
//     problem, the AgentDispatch.runKeepalive pattern lifts straight
//     across.
//
// The legacy AgentDispatch.Connect path remains live alongside this
// one. Operators choose per-deployment which transport the agent
// uses by toggling the corresponding control-plane URL flag.
// Migration off AgentDispatch is a separate decision once
// AttachDrivers has burn-in.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/openweft/weft-proto/agentv1"
)

// attachDriversSession tracks one AttachDrivers stream. Keyed by
// host UUID in the server's sessions map. The fields mirror
// agentDispatchServer's agentSession ; same lifecycle conventions
// (register-on-Init, deregister-on-EOF, supersede-by-reconnect
// cancels the predecessor).
type attachDriversSession struct {
	hostUUID    string
	stream      agentv1.AgentControlPlane_AttachDriversServer
	connectedAt time.Time
	// send funnels outbound frames into the stream. gRPC bidi
	// streams aren't safe for concurrent writes ; all senders
	// enqueue here and the dedicated sender goroutine drains.
	send chan *agentv1.AttachDriversFrame
	// nextCallID hands out monotonically-increasing call_ids
	// scoped to this session. Atomic so concurrent Dispatch
	// callers don't collide. Starts at 1 so 0 stays available
	// as a sentinel meaning "no call_id assigned yet".
	nextCallID atomic.Uint64
	// pending maps active call_ids to the goroutine awaiting
	// the matching DriverDispatchResult.
	pending pendingDispatchResults
	// cancel tears down the per-session goroutines on
	// supersede-by-reconnect. Set by AttachDrivers before
	// launching the sender/receiver.
	cancel context.CancelFunc
}

// pendingDispatchResults is the AttachDrivers analog of
// agent_dispatch.go's pendingReplies — call_id → reply chan,
// protected by a mutex. Buffered (cap 1) chans so deliver never
// blocks even when the caller already gave up.
type pendingDispatchResults struct {
	mu sync.Mutex
	m  map[uint64]chan *agentv1.DriverDispatchResult
}

func (p *pendingDispatchResults) register(id uint64, ch chan *agentv1.DriverDispatchResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[uint64]chan *agentv1.DriverDispatchResult)
	}
	p.m[id] = ch
}

func (p *pendingDispatchResults) deliver(id uint64, result *agentv1.DriverDispatchResult) bool {
	p.mu.Lock()
	ch, ok := p.m[id]
	if ok {
		delete(p.m, id)
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- result:
	default:
	}
	return true
}

// drainAll wakes every pending caller with a synthetic error
// Result when the session dies. The Error field is the
// abort-reason string ; CallId is preserved so the caller can
// still correlate.
func (p *pendingDispatchResults) drainAll(reason string) {
	p.mu.Lock()
	pending := p.m
	p.m = nil
	p.mu.Unlock()
	for id, ch := range pending {
		select {
		case ch <- &agentv1.DriverDispatchResult{CallId: id, ErrorMessage: reason}:
		default:
		}
	}
}

// initAttachDriversRegistry initialises the per-server session map.
// Called once from the server constructor — keeps the zero-value
// safety net (a server constructed without this call still works
// for the Init-only observability path, just no Dispatch).
func (s *agentControlPlaneServer) initAttachDriversRegistry() {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.attachSessions == nil {
		s.attachSessions = make(map[string]*attachDriversSession)
	}
}

// registerAttachSession installs the session keyed on host UUID,
// returning the previous session (if any) so the caller can drain
// it. Mirrors agentDispatchServer.register exactly.
func (s *agentControlPlaneServer) registerAttachSession(sess *attachDriversSession) *attachDriversSession {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if s.attachSessions == nil {
		s.attachSessions = make(map[string]*attachDriversSession)
	}
	existing := s.attachSessions[sess.hostUUID]
	s.attachSessions[sess.hostUUID] = sess
	return existing
}

// deregisterAttachSession removes the session iff it's still the
// current one (didn't lose the supersede race). Returns true on
// actual removal so callers can fire post-disconnect hooks at the
// right time.
func (s *agentControlPlaneServer) deregisterAttachSession(sess *attachDriversSession) bool {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	if cur, ok := s.attachSessions[sess.hostUUID]; ok && cur == sess {
		delete(s.attachSessions, sess.hostUUID)
		return true
	}
	return false
}

// AttachSessionCount returns how many AttachDrivers streams are
// currently registered. Used by `weft host ls` to show which
// transport(s) a host is reachable on, and by tests to assert the
// lifecycle.
func (s *agentControlPlaneServer) AttachSessionCount() int {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	return len(s.attachSessions)
}

// AttachConnectedHostUUIDs returns a snapshot of host UUIDs with
// an open AttachDrivers stream. Order is not guaranteed.
func (s *agentControlPlaneServer) AttachConnectedHostUUIDs() []string {
	s.attachMu.Lock()
	defer s.attachMu.Unlock()
	out := make([]string, 0, len(s.attachSessions))
	for u := range s.attachSessions {
		out = append(out, u)
	}
	return out
}

// Dispatch sends a DriverDispatchCall to the named host's
// AttachDrivers stream + waits for the matching Result. Mirrors
// agentDispatchServer.Dispatch's contract :
//
//   - codes.Unavailable when no AttachDrivers session exists for
//     the host (operator hasn't enabled the new transport, or the
//     stream is mid-reconnect).
//   - codes.DeadlineExceeded / Canceled when ctx fires before the
//     reply arrives.
//   - codes.Aborted when the session drains while the call is
//     in flight (agent disconnected — the caller may retry, or
//     fall back to AgentDispatch.Connect).
//
// The caller MUST NOT pre-set call.CallId ; the server stamps a
// fresh one before sending so reply correlation works. Returning
// the Result with Error populated is NOT a Go-level error : the
// caller inspects Result.Error to distinguish protocol errors
// (no reply, transport dropped) from method errors (driver said
// "no such VM").
func (s *agentControlPlaneServer) Dispatch(ctx context.Context, hostUUID string, call *agentv1.DriverDispatchCall) (*agentv1.DriverDispatchResult, error) {
	if call == nil {
		return nil, status.Error(codes.InvalidArgument, "nil DriverDispatchCall")
	}
	s.attachMu.Lock()
	sess, ok := s.attachSessions[hostUUID]
	s.attachMu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "no AttachDrivers session for host %s", hostUUID)
	}

	// Server-assigned call_id : monotonic per-session counter so
	// concurrent Dispatch callers never collide. Overrides
	// anything the caller pre-set ; that field is server-owned.
	id := sess.nextCallID.Add(1)
	call.CallId = id

	replyCh := make(chan *agentv1.DriverDispatchResult, 1)
	sess.pending.register(id, replyCh)

	frame := &agentv1.AttachDriversFrame{
		Body: &agentv1.AttachDriversFrame_Dispatch{Dispatch: call},
	}
	select {
	case sess.send <- frame:
	case <-ctx.Done():
		// Caller bailed before we even queued the send. Unregister
		// via deliver-and-drop so the pending table doesn't leak.
		sess.pending.deliver(id, nil)
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	select {
	case result := <-replyCh:
		if result == nil {
			return nil, status.Errorf(codes.Aborted, "AttachDrivers session aborted for host %s", hostUUID)
		}
		return result, nil
	case <-ctx.Done():
		// Leave the entry registered ; if a result lands later
		// the receiver logs "unknown call_id" and drops it. We
		// can't unregister atomically without racing the receiver.
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// runAttachSender drains sess.send → stream.Send. Exits on ctx
// cancel or Send error ; pushes the error to errCh so the parent
// AttachDrivers handler can terminate the session.
func (s *agentControlPlaneServer) runAttachSender(ctx context.Context, sess *attachDriversSession, errCh chan<- error) {
	for {
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		case frame, ok := <-sess.send:
			if !ok {
				errCh <- fmt.Errorf("session send chan closed")
				return
			}
			if err := sess.stream.Send(frame); err != nil {
				errCh <- err
				return
			}
		}
	}
}

// runAttachReceiver reads inbound frames + routes Result frames
// to the pending table. Other frame kinds (a stray Init, a
// Disconnect) fall through to the same observability path the
// pre-dispatch handler used (PlatformEvent forward) so existing
// subscribers keep seeing them — wired by the caller passing
// `forwardFrame` here. Exits on Recv error.
func (s *agentControlPlaneServer) runAttachReceiver(sess *attachDriversSession, forwardFrame func(string, map[string]string), errCh chan<- error) {
	for {
		msg, err := sess.stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		switch body := msg.Body.(type) {
		case *agentv1.AttachDriversFrame_Result:
			if body.Result == nil {
				continue
			}
			if !sess.pending.deliver(body.Result.CallId, body.Result) {
				// Reply for an unknown call_id : either the caller
				// timed out + already cleaned up, or the agent is
				// confused. Log + drop.
				logger.Printf("attach-drivers: %s result for unknown call_id %d (caller already timed out?)",
					sess.hostUUID, body.Result.CallId)
			}
		case *agentv1.AttachDriversFrame_Init:
			// Duplicate Init mid-stream : observability forward only,
			// matches the pre-dispatch behaviour.
			if forwardFrame != nil {
				forwardFrame("init", nil)
			}
		case *agentv1.AttachDriversFrame_Dispatch:
			// Client→server Dispatch frames don't exist in the
			// half-duplex use today, but log + forward defensively.
			if forwardFrame != nil {
				forwardFrame("dispatch", nil)
			}
		case *agentv1.AttachDriversFrame_Disconnect:
			reason := ""
			if body.Disconnect != nil {
				reason = body.Disconnect.Reason
			}
			if forwardFrame != nil {
				forwardFrame("disconnect", map[string]string{"reason": reason})
			}
			logger.Printf("attach-drivers: %s disconnect : %s", sess.hostUUID, reason)
			errCh <- nil // clean close — handler treats nil as server_eof
			return
		}
	}
}
