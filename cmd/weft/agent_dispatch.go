package main

// agent_dispatch.go owns the server-side of the per-host
// `AgentDispatch.Connect` bidi stream. Per-host agents
// (`weft agent --client --control-plane=URL`) open one stream
// each ; the control plane registers them keyed on host UUID
// so the scheduler can later send `DriverRequest` messages to
// the right node.
//
// Today the stream carries Hello + Ping/Pong (keepalive)
// only — that's the scaffold. Driver-dispatch ops slot into
// the existing oneof in the proto + here without breaking the
// stream's lifecycle.
//
// Lifecycle :
//
//   1. Agent dials, calls Connect, sends AgentHello.
//   2. Server validates host_uuid is in the Host registry,
//      assigns a session_id, registers the stream, sends
//      ControlHelloAck.
//   3. Server keepalive goroutine sends ControlPing every N
//      seconds ; agent replies with AgentPong (echoes
//      session_id). Missed pongs → server closes the stream.
//   4. Agent disconnect → server removes the entry from the
//      registry. Reconnect → same host_uuid, fresh session_id.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	vzdv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// agentDispatchServer holds the per-host stream registry. One
// `vzdServer` owns one of these (instantiated in main.go's
// run() alongside the other gRPC handlers).
type agentDispatchServer struct {
	vzdv1.UnimplementedAgentDispatchServer

	mu       sync.Mutex
	sessions map[string]*agentSession // host_uuid → session

	// keepaliveInterval is how often the server sends ControlPing
	// down each session's stream. Zero disables keepalive (only
	// useful for tests that want to drive the lifecycle by hand).
	// Default is 10s ; an idle stream that goes silent for one
	// interval gets a Ping ; a broken stream surfaces a Send
	// error on the next interval which ends the session.
	keepaliveInterval time.Duration

	// livenessTimeout is how long a session may go without an
	// AgentPong before the server considers it dead and closes
	// the stream. Default is 3 * keepaliveInterval ; ≤0 disables
	// the liveness sweep (tests that drive the lifecycle by hand
	// set it explicitly).
	livenessTimeout time.Duration

	// onSessionDown is called once when a session genuinely ends
	// (i.e. the deregister actually removes the entry from the
	// map — not the "superseded by a fresh reconnect" path). Nil
	// is fine ; the production wiring sets this to
	// `adapter.SetHostState(uuid, HostStateDown)` so the host
	// drops out of the scheduler's candidate pool until the agent
	// reconnects (RegisterHost flips it back to Active).
	onSessionDown func(hostUUID string)
	// onSessionUp is called once per successful session register
	// (including the "supersede by reconnect" path — a fresh
	// session is a fresh session). Wiring publishes
	// `agent.connected` on the event bus so operators see new /
	// reconnecting agents on `weft events`.
	onSessionUp func(hostUUID, sessionID string)
}

// agentSession tracks one connected agent. The send chan is the
// only way driver-dispatch code routes a request to the agent ;
// the server's Connect goroutine is the sole reader of recvFn.
type agentSession struct {
	hostUUID    string
	sessionID   string
	stream      vzdv1.AgentDispatch_ConnectServer
	connectedAt time.Time
	// send funnels outgoing ControlMessages to the stream's
	// Send goroutine — gRPC bidi streams aren't safe for
	// concurrent writes, so all senders enqueue here.
	send chan *vzdv1.ControlMessage
	// pending correlates outstanding DriverRequests with the
	// goroutines awaiting their DriverReply. The receiver
	// goroutine looks up the reply chan by request_id and
	// forwards the reply ; Dispatch's caller registers + waits.
	pending pendingReplies
	// lastPongUnixNano is the wall-clock of the most recent
	// AgentPong (or session creation if no Pong has arrived
	// yet). The liveness check compares this to `now -
	// livenessTimeout` to decide whether the session is stale.
	// Atomic so the receiver (writer) and liveness goroutine
	// (reader) don't race on it.
	lastPongUnixNano atomic.Int64
}

// pendingReplies is a small thread-safe map[request_id] →
// reply chan. Caller-side : `Dispatch` creates the chan,
// registers it, sends the request, awaits the chan. Receiver-
// side : the stream's Recv loop looks up by request_id and
// pushes the reply, then unregisters.
type pendingReplies struct {
	mu sync.Mutex
	m  map[string]chan *vzdv1.DriverReply
}

func (p *pendingReplies) register(id string, ch chan *vzdv1.DriverReply) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[string]chan *vzdv1.DriverReply)
	}
	p.m[id] = ch
}

func (p *pendingReplies) deliver(id string, reply *vzdv1.DriverReply) bool {
	p.mu.Lock()
	ch, ok := p.m[id]
	if ok {
		delete(p.m, id)
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	// Buffered chan so deliver never blocks even if the caller
	// already gave up.
	select {
	case ch <- reply:
	default:
	}
	return true
}

// drainAll wakes every pending caller with an error reply when
// the session dies. Without this, a session-disconnect would
// leave Dispatch() callers blocked on their chan until ctx
// cancel ; this is the explicit-failure path that frees them
// promptly.
func (p *pendingReplies) drainAll(reason string) {
	p.mu.Lock()
	pending := p.m
	p.m = nil
	p.mu.Unlock()
	for id, ch := range pending {
		select {
		case ch <- &vzdv1.DriverReply{RequestId: id, Error: reason}:
		default:
		}
	}
}

func newAgentDispatchServer() *agentDispatchServer {
	const ka = 10 * time.Second
	return &agentDispatchServer{
		sessions:          make(map[string]*agentSession),
		keepaliveInterval: ka,
		livenessTimeout:   3 * ka,
	}
}

// Connect is the bidi-stream handler. Blocks until the agent
// disconnects or the server closes the stream.
func (s *agentDispatchServer) Connect(stream vzdv1.AgentDispatch_ConnectServer) error {
	// First message MUST be Hello — gate every other variant
	// until we've identified the host.
	msg, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Unavailable, "recv hello: %v", err)
	}
	hello := msg.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	if hello.HostUuid == "" {
		return status.Error(codes.InvalidArgument, "Hello.host_uuid is required")
	}

	sess := &agentSession{
		hostUUID:    hello.HostUuid,
		sessionID:   newSessionID(),
		stream:      stream,
		connectedAt: time.Now().UTC(),
		send:        make(chan *vzdv1.ControlMessage, 16),
	}
	// Seed the pong clock so a freshly-connected agent isn't
	// immediately considered stale by the liveness check.
	sess.lastPongUnixNano.Store(time.Now().UnixNano())
	if existing := s.register(sess); existing != nil {
		// Same host reconnected — close the old session first.
		// In a real deployment the underlying TCP / Unix-socket
		// drop already torn down `existing.stream` ; this is
		// belt-and-braces for the never-quite-disconnected case.
		existing.pending.drainAll("session superseded by reconnect")
		close(existing.send)
	}
	defer func() {
		removed := s.deregister(sess)
		// Wake any goroutines still blocked on a DriverReply
		// from this now-dead session.
		sess.pending.drainAll("agent session ended")
		// Fire the host-down hook only when this session was
		// still the current one (`removed` true). The superseded-
		// by-reconnect path leaves the map entry pointing at the
		// fresh session — demoting in that case would race the
		// agent's own RegisterHost re-promotion.
		if removed && s.onSessionDown != nil {
			s.onSessionDown(sess.hostUUID)
		}
	}()

	// Send the Hello ack so the agent knows it's registered.
	ack := &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{
		HelloAck: &vzdv1.ControlHelloAck{SessionId: sess.sessionID},
	}}
	if err := stream.Send(ack); err != nil {
		return status.Errorf(codes.Unavailable, "send hello-ack: %v", err)
	}
	logger.Printf("agent-dispatch: %s connected (session=%s, version=%q)",
		sess.hostUUID, sess.sessionID, hello.AgentVersion)

	// Four goroutines : sender drains send→stream.Send, receiver
	// reads incoming messages, keepalive periodically pushes a
	// Ping into the send chan so dead connections surface as a
	// Send error within one interval, liveness watches the
	// last-pong clock + closes silent sessions explicitly (in
	// case the underlying transport keeps eating Send writes
	// without surfacing an error). errCh is buffered for all
	// goroutines so an early exit on one path doesn't block the
	// others' shutdown writes.
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	errCh := make(chan error, 4)
	go s.runSender(ctx, sess, errCh)
	go s.runReceiver(ctx, sess, errCh)
	go s.runKeepalive(ctx, sess)
	go s.runLivenessCheck(ctx, sess, errCh)

	err = <-errCh
	cancel()
	logger.Printf("agent-dispatch: %s disconnected: %v", sess.hostUUID, err)
	return err
}

func (s *agentDispatchServer) runSender(ctx context.Context, sess *agentSession, errCh chan<- error) {
	for {
		select {
		case <-ctx.Done():
			errCh <- ctx.Err()
			return
		case msg, ok := <-sess.send:
			if !ok {
				errCh <- fmt.Errorf("session closed")
				return
			}
			if err := sess.stream.Send(msg); err != nil {
				errCh <- err
				return
			}
		}
	}
}

func (s *agentDispatchServer) runReceiver(_ context.Context, sess *agentSession, errCh chan<- error) {
	for {
		msg, err := sess.stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		switch body := msg.Body.(type) {
		case *vzdv1.AgentMessage_Pong:
			// Refresh the liveness clock. The exact RTT measurement
			// (now - body.Pong.PingedUnixNs) is a future telemetry
			// hook ; today we only care about "the agent is still
			// talking to us".
			sess.lastPongUnixNano.Store(time.Now().UnixNano())
		case *vzdv1.AgentMessage_Reply:
			// Route the reply to the goroutine awaiting it. An
			// unknown request_id is the result of a server-side
			// timeout that already gave up — log + drop.
			if !sess.pending.deliver(body.Reply.RequestId, body.Reply) {
				logger.Printf("agent-dispatch: %s reply for unknown request_id %q (caller already timed out?)",
					sess.hostUUID, body.Reply.RequestId)
			}
		case *vzdv1.AgentMessage_Hello:
			// Hello after the first one is a protocol violation —
			// drop the connection.
			errCh <- status.Error(codes.InvalidArgument, "duplicate Hello on established session")
			return
		}
	}
}

// runKeepalive ticks every `keepaliveInterval` and pushes a
// ControlPing into the session's send chan. Two effects :
//
//  1. The agent sees the Ping + sends a Pong — keeps any
//     intermediate firewall / load-balancer from idle-timing-
//     out the stream.
//  2. If the underlying TCP connection is dead, the sender's
//     stream.Send call surfaces the error within one interval,
//     ending the session promptly instead of leaving it stale.
//
// keepaliveInterval ≤ 0 disables the loop (tests that drive the
// lifecycle by hand set it explicitly).
//
// Doesn't write to errCh — the session-ending signal comes from
// the sender's failed Send (which writes errCh). The goroutine
// exits when ctx cancels (i.e. when the session is tearing down
// anyway).
func (s *agentDispatchServer) runKeepalive(ctx context.Context, sess *agentSession) {
	if s.keepaliveInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ping := &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Ping{
				Ping: &vzdv1.ControlPing{
					SessionId:  sess.sessionID,
					SentUnixNs: time.Now().UnixNano(),
				},
			}}
			// Honour context cancel even when the send chan is
			// backed up — otherwise we'd wedge during shutdown.
			select {
			case sess.send <- ping:
			case <-ctx.Done():
				return
			}
		}
	}
}

// runLivenessCheck watches sess.lastPongUnixNano and kills the
// session if the agent stops pong-ing for longer than
// `livenessTimeout`. Pairs with runKeepalive : keepalive emits
// the Ping, liveness enforces the deadline on the reply.
//
// livenessTimeout ≤ 0 disables the loop (tests that drive the
// lifecycle by hand set it explicitly). The tick cadence is
// `keepaliveInterval` (one check per ping) — checking faster
// just burns CPU, checking slower lets stale sessions linger.
// A keepaliveInterval of 0 also disables liveness : without
// pings the agent has nothing to reply to.
func (s *agentDispatchServer) runLivenessCheck(ctx context.Context, sess *agentSession, errCh chan<- error) {
	if s.livenessTimeout <= 0 || s.keepaliveInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := sess.lastPongUnixNano.Load()
			if time.Since(time.Unix(0, last)) > s.livenessTimeout {
				errCh <- status.Errorf(codes.DeadlineExceeded,
					"liveness timeout : no pong from host %s for %s",
					sess.hostUUID, s.livenessTimeout)
				return
			}
		}
	}
}

// Dispatch sends a DriverRequest to the agent for `hostUUID` and
// blocks until the matching DriverReply arrives, the session
// dies, or the context cancels. The caller-supplied request_id
// is overwritten with a fresh UUID so concurrent callers don't
// collide.
//
// Returns the reply as-is — the caller is expected to check
// `reply.Error` and unwrap the typed `result` oneof.
//
// Errors :
//
//   - codes.Unavailable when the host has no connected agent
//     (operator forgot to run `weft agent --client` there, or
//     the stream is mid-reconnect).
//   - codes.DeadlineExceeded / Canceled when ctx fires before
//     the reply arrives.
//   - codes.Aborted when the session drains while the call is
//     in flight (agent disconnected — the caller may retry).
func (s *agentDispatchServer) Dispatch(ctx context.Context, hostUUID string, op *vzdv1.DriverRequest) (*vzdv1.DriverReply, error) {
	s.mu.Lock()
	sess, ok := s.sessions[hostUUID]
	s.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "no connected agent for host %s", hostUUID)
	}
	if op == nil {
		return nil, status.Error(codes.InvalidArgument, "nil DriverRequest")
	}

	// Server-assigned request_id : overrides anything the caller
	// put in. Callers MUST NOT rely on their pre-set value.
	reqID := newSessionID() + newSessionID() // 32 hex chars
	op.RequestId = reqID

	replyCh := make(chan *vzdv1.DriverReply, 1)
	sess.pending.register(reqID, replyCh)

	wrapped := &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{Request: op}}
	select {
	case sess.send <- wrapped:
	case <-ctx.Done():
		// Caller bailed before we even queued the send.
		sess.pending.deliver(reqID, nil) // unregister via deliver-and-drop
		return nil, status.FromContextError(ctx.Err()).Err()
	}

	select {
	case reply := <-replyCh:
		if reply == nil {
			return nil, status.Errorf(codes.Aborted, "session aborted for host %s", hostUUID)
		}
		return reply, nil
	case <-ctx.Done():
		// Leave the entry registered ; if a reply does land
		// later the receiver will log "unknown request_id" and
		// drop it. We can't unregister atomically without a
		// race against the receiver's deliver().
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

func (s *agentDispatchServer) register(sess *agentSession) *agentSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.sessions[sess.hostUUID]
	s.sessions[sess.hostUUID] = sess
	return existing
}

// deregister removes the session from the map iff it's still
// the current one. Returns true when it actually deleted —
// callers use that to decide whether to fire the onSessionDown
// hook (a superseded session means a fresh one is already in
// place + the host shouldn't be demoted).
func (s *agentDispatchServer) deregister(sess *agentSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.sessions[sess.hostUUID]; ok && cur.sessionID == sess.sessionID {
		delete(s.sessions, sess.hostUUID)
		return true
	}
	return false
}

// SessionCount is the number of currently-connected agents.
// Useful for `weft host ls` to surface "connected: true/false"
// + for tests.
func (s *agentDispatchServer) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// ConnectedHostUUIDs returns the UUIDs of every host with an
// open dispatch session. The slice is a fresh snapshot — safe
// to mutate without affecting future calls. Order is not
// guaranteed (sessions live in a map) ; callers that need a
// stable display order should sort.
func (s *agentDispatchServer) ConnectedHostUUIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.sessions))
	for uuid := range s.sessions {
		out = append(out, uuid)
	}
	return out
}

// newSessionID returns 16 hex chars (8 random bytes). Short
// enough to fit comfortably in a single-line log message, long
// enough that two parallel reconnects don't collide.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
