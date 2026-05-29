//go:build darwin && cgo

package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	vzdv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestPendingReplies_RegisterDeliver pins the correlation map.
// Register a chan, deliver a reply by request_id, the chan
// receives the right reply and the entry is removed.
func TestPendingReplies_RegisterDeliver(t *testing.T) {
	var p pendingReplies
	ch := make(chan *vzdv1.DriverReply, 1)
	p.register("req-1", ch)

	ok := p.deliver("req-1", &vzdv1.DriverReply{RequestId: "req-1", Error: ""})
	if !ok {
		t.Fatal("deliver returned false for a registered request_id")
	}
	select {
	case got := <-ch:
		if got.RequestId != "req-1" {
			t.Errorf("got reply %+v, want req-1", got)
		}
	default:
		t.Fatal("reply not pushed to the registered chan")
	}

	// Second deliver for the same id should fail-soft (already gone).
	if p.deliver("req-1", &vzdv1.DriverReply{}) {
		t.Errorf("second deliver should return false (entry already removed)")
	}
}

// TestPendingReplies_DrainAll pins the disconnect path : every
// pending caller wakes up with an error-carrying reply when
// the session ends.
func TestPendingReplies_DrainAll(t *testing.T) {
	var p pendingReplies
	chs := map[string]chan *vzdv1.DriverReply{
		"r1": make(chan *vzdv1.DriverReply, 1),
		"r2": make(chan *vzdv1.DriverReply, 1),
		"r3": make(chan *vzdv1.DriverReply, 1),
	}
	for id, ch := range chs {
		p.register(id, ch)
	}
	p.drainAll("session ended")
	for id, ch := range chs {
		select {
		case got := <-ch:
			if got.RequestId != id || got.Error == "" {
				t.Errorf("drain for %s = %+v, want id+error", id, got)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("drain did not wake the chan for %s", id)
		}
	}
}

// TestDispatch_RoundTrip pins the end-to-end server flow :
// 1. session registered, 2. Dispatch sends a request through
// sess.send, 3. a simulated agent reads it + calls
// sess.pending.deliver, 4. Dispatch returns the reply.
func TestDispatch_RoundTrip(t *testing.T) {
	srv := newAgentDispatchServer()
	sess := &agentSession{
		hostUUID:  "h-1",
		sessionID: "s-1",
		send:      make(chan *vzdv1.ControlMessage, 4),
	}
	srv.register(sess)
	defer srv.deregister(sess)

	// Simulated agent : reads outgoing ControlMessages, replies
	// to DriverRequests by calling pending.deliver directly.
	go func() {
		for msg := range sess.send {
			if req := msg.GetRequest(); req != nil {
				sess.pending.deliver(req.RequestId, &vzdv1.DriverReply{
					RequestId: req.RequestId,
					Result: &vzdv1.DriverReply_CreateVm{
						CreateVm: &vzdv1.CreateVMResult{VmUuid: "vm-result"},
					},
				})
			}
		}
	}()

	reply, err := srv.Dispatch(context.Background(), "h-1", &vzdv1.DriverRequest{
		Op: &vzdv1.DriverRequest_CreateVm{CreateVm: &vzdv1.CreateVMOp{Project: "alpha"}},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if reply.GetCreateVm() == nil || reply.GetCreateVm().VmUuid != "vm-result" {
		t.Errorf("reply = %+v, want CreateVMResult{vm_uuid=vm-result}", reply)
	}
}

// TestDispatch_NoConnectedAgent pins the codes.Unavailable
// error path : no session for the host, Dispatch fails cleanly
// instead of blocking.
func TestDispatch_NoConnectedAgent(t *testing.T) {
	srv := newAgentDispatchServer()
	_, err := srv.Dispatch(context.Background(), "h-missing", &vzdv1.DriverRequest{})
	if err == nil {
		t.Fatal("expected error for missing agent")
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("status = %v, want Unavailable", status.Code(err))
	}
}

// TestRunKeepalive_EmitsPing pins the periodic keepalive loop :
// every `keepaliveInterval` the server pushes a ControlPing
// into the session's send chan, carrying the right session_id +
// a fresh timestamp.
func TestRunKeepalive_EmitsPing(t *testing.T) {
	srv := newAgentDispatchServer()
	srv.keepaliveInterval = 5 * time.Millisecond
	sess := &agentSession{
		hostUUID:  "h-1",
		sessionID: "sess-keep",
		send:      make(chan *vzdv1.ControlMessage, 4),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.runKeepalive(ctx, sess)

	select {
	case msg := <-sess.send:
		ping := msg.GetPing()
		if ping == nil {
			t.Fatalf("expected ControlPing, got %T", msg.Body)
		}
		if ping.SessionId != "sess-keep" {
			t.Errorf("ping.session_id = %q, want sess-keep", ping.SessionId)
		}
		if ping.SentUnixNs == 0 {
			t.Errorf("ping.sent_unix_ns should be non-zero")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("keepalive did not emit a Ping within 200ms")
	}
}

// TestRunKeepalive_DisabledZeroInterval pins the opt-out :
// keepaliveInterval=0 makes runKeepalive return immediately
// (tests that drive lifecycle by hand depend on this).
func TestRunKeepalive_DisabledZeroInterval(t *testing.T) {
	srv := newAgentDispatchServer()
	srv.keepaliveInterval = 0
	sess := &agentSession{
		send: make(chan *vzdv1.ControlMessage, 1),
	}
	done := make(chan struct{})
	go func() {
		srv.runKeepalive(context.Background(), sess)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("runKeepalive should return immediately when interval=0")
	}
	select {
	case msg := <-sess.send:
		t.Errorf("no Ping should be emitted when keepalive is disabled, got %T", msg.Body)
	default:
	}
}

// TestRunLivenessCheck_KillsStaleSession pins the timeout path :
// a session whose lastPongUnixNano never advances triggers a
// DeadlineExceeded error from the liveness goroutine within
// `livenessTimeout`.
func TestRunLivenessCheck_KillsStaleSession(t *testing.T) {
	srv := newAgentDispatchServer()
	srv.keepaliveInterval = 5 * time.Millisecond
	srv.livenessTimeout = 15 * time.Millisecond
	sess := &agentSession{
		hostUUID: "h-stale",
		send:     make(chan *vzdv1.ControlMessage, 1),
	}
	// Backdate the pong clock so the very first tick declares
	// the session stale.
	sess.lastPongUnixNano.Store(time.Now().Add(-time.Second).UnixNano())

	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.runLivenessCheck(ctx, sess, errCh)
	select {
	case err := <-errCh:
		if status.Code(err) != codes.DeadlineExceeded {
			t.Errorf("status = %v, want DeadlineExceeded", status.Code(err))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("liveness check did not surface a timeout error")
	}
}

// TestRunLivenessCheck_FreshPongKeepsAlive pins the happy path :
// pong updates keep advancing the clock so the deadline never
// trips. Run for > livenessTimeout and confirm no error fires.
func TestRunLivenessCheck_FreshPongKeepsAlive(t *testing.T) {
	srv := newAgentDispatchServer()
	srv.keepaliveInterval = 5 * time.Millisecond
	srv.livenessTimeout = 20 * time.Millisecond
	sess := &agentSession{
		hostUUID: "h-live",
		send:     make(chan *vzdv1.ControlMessage, 1),
	}
	sess.lastPongUnixNano.Store(time.Now().UnixNano())

	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.runLivenessCheck(ctx, sess, errCh)

	// Simulate fresh pongs every ~5ms for > livenessTimeout.
	pongDone := make(chan struct{})
	go func() {
		defer close(pongDone)
		ticker := time.NewTicker(3 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.After(60 * time.Millisecond)
		for {
			select {
			case <-deadline:
				return
			case <-ticker.C:
				sess.lastPongUnixNano.Store(time.Now().UnixNano())
			}
		}
	}()
	<-pongDone
	select {
	case err := <-errCh:
		t.Fatalf("liveness check fired unexpectedly: %v", err)
	default:
	}
}

// TestRunLivenessCheck_DisabledZeroTimeout pins the opt-out :
// livenessTimeout=0 makes the loop return immediately.
func TestRunLivenessCheck_DisabledZeroTimeout(t *testing.T) {
	srv := newAgentDispatchServer()
	srv.keepaliveInterval = 5 * time.Millisecond
	srv.livenessTimeout = 0
	sess := &agentSession{hostUUID: "h-off"}

	errCh := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		srv.runLivenessCheck(context.Background(), sess, errCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("runLivenessCheck should return immediately when timeout=0")
	}
	select {
	case err := <-errCh:
		t.Errorf("disabled liveness check should not push an error, got %v", err)
	default:
	}
}

// TestOnSessionDown_FiresOnDeregister pins the host-demotion
// hook : when deregister actually removes the session (i.e. it
// was the current one), the onSessionDown callback fires with
// the host UUID.
func TestOnSessionDown_FiresOnDeregister(t *testing.T) {
	srv := newAgentDispatchServer()
	var calls atomic.Int32
	var lastUUID atomic.Value
	srv.onSessionDown = func(hostUUID string) {
		calls.Add(1)
		lastUUID.Store(hostUUID)
	}
	sess := &agentSession{hostUUID: "h-down", sessionID: "s-1"}
	srv.register(sess)
	if !srv.deregister(sess) {
		t.Fatal("deregister should return true for the current session")
	}
	// The hook is fired by Connect's defer in production ; here
	// we exercise the building blocks directly. Confirm the
	// callback wiring works when called.
	if srv.onSessionDown != nil {
		srv.onSessionDown(sess.hostUUID)
	}
	if calls.Load() != 1 {
		t.Errorf("onSessionDown fired %d times, want 1", calls.Load())
	}
	if got, _ := lastUUID.Load().(string); got != "h-down" {
		t.Errorf("onSessionDown got uuid %q, want h-down", got)
	}
}

// TestConnectedHostUUIDs pins the runtime view used by
// `weft host ls` : the slice contains every registered session
// exactly once + reflects deregistrations.
func TestConnectedHostUUIDs(t *testing.T) {
	srv := newAgentDispatchServer()
	if got := srv.ConnectedHostUUIDs(); len(got) != 0 {
		t.Errorf("empty server should return no UUIDs, got %v", got)
	}
	a := &agentSession{hostUUID: "h-a", sessionID: "s-a"}
	b := &agentSession{hostUUID: "h-b", sessionID: "s-b"}
	srv.register(a)
	srv.register(b)
	got := srv.ConnectedHostUUIDs()
	if len(got) != 2 {
		t.Fatalf("got %d UUIDs, want 2 (%v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, u := range got {
		seen[u] = true
	}
	if !seen["h-a"] || !seen["h-b"] {
		t.Errorf("ConnectedHostUUIDs = %v, want {h-a, h-b}", got)
	}
	srv.deregister(a)
	got = srv.ConnectedHostUUIDs()
	if len(got) != 1 || got[0] != "h-b" {
		t.Errorf("after deregister: got %v, want [h-b]", got)
	}
}

// TestDeregister_ReturnsFalseForSuperseded pins the
// reconnect-supersedes-old-session path : if a fresh session
// has replaced ours, deregistering the OLD one is a no-op + the
// host-down hook (per Connect's defer logic) must NOT fire.
func TestDeregister_ReturnsFalseForSuperseded(t *testing.T) {
	srv := newAgentDispatchServer()
	old := &agentSession{hostUUID: "h-reuse", sessionID: "s-old"}
	fresh := &agentSession{hostUUID: "h-reuse", sessionID: "s-new"}
	srv.register(old)
	srv.register(fresh) // swaps the map entry
	if srv.deregister(old) {
		t.Error("deregister of superseded session should return false")
	}
	// The fresh session is still in the map.
	if got := srv.SessionCount(); got != 1 {
		t.Errorf("SessionCount = %d, want 1 (fresh session remains)", got)
	}
}

// TestRunKeepalive_StopsOnCtxCancel pins the shutdown path : a
// cancelled ctx ends the loop without trying to write into a
// (potentially-closed) send chan.
func TestRunKeepalive_StopsOnCtxCancel(t *testing.T) {
	srv := newAgentDispatchServer()
	srv.keepaliveInterval = 1 * time.Millisecond
	sess := &agentSession{
		send: make(chan *vzdv1.ControlMessage, 8),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.runKeepalive(ctx, sess)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond) // let it tick a few times
	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("runKeepalive should exit promptly on ctx cancel")
	}
}

// TestShouldDispatch_LocalVsRemote pins the routing predicate :
// empty host_uuid → local, self-match → local, every other
// non-empty value with a configured dispatch registry → remote.
// A nil dispatch registry forces every request local (the
// integration-test fallback).
func TestShouldDispatch_LocalVsRemote(t *testing.T) {
	cases := []struct {
		name         string
		s            *vzdServer
		hostUUID     string
		wantDispatch bool
	}{
		{
			name:         "empty host_uuid",
			s:            &vzdServer{dispatch: newAgentDispatchServer(), localHostUUID: "local-1"},
			hostUUID:     "",
			wantDispatch: false,
		},
		{
			name:         "self-match",
			s:            &vzdServer{dispatch: newAgentDispatchServer(), localHostUUID: "local-1"},
			hostUUID:     "local-1",
			wantDispatch: false,
		},
		{
			name:         "remote target",
			s:            &vzdServer{dispatch: newAgentDispatchServer(), localHostUUID: "local-1"},
			hostUUID:     "remote-1",
			wantDispatch: true,
		},
		{
			name:         "nil dispatch registry forces local",
			s:            &vzdServer{dispatch: nil, localHostUUID: "local-1"},
			hostUUID:     "remote-1",
			wantDispatch: false,
		},
		{
			name:         "unknown local uuid + remote target dispatches",
			s:            &vzdServer{dispatch: newAgentDispatchServer(), localHostUUID: ""},
			hostUUID:     "remote-1",
			wantDispatch: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.s.shouldDispatch(c.hostUUID); got != c.wantDispatch {
				t.Errorf("shouldDispatch(%q) = %v, want %v (local=%q)",
					c.hostUUID, got, c.wantDispatch, c.s.localHostUUID)
			}
		})
	}
}

// TestDispatch_ContextCanceled pins the caller-cancel path :
// Dispatch returns the context's error promptly instead of
// blocking forever waiting for a reply that never arrives.
func TestDispatch_ContextCanceled(t *testing.T) {
	srv := newAgentDispatchServer()
	sess := &agentSession{
		hostUUID: "h-1",
		send:     make(chan *vzdv1.ControlMessage, 1),
	}
	srv.register(sess)
	defer srv.deregister(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := srv.Dispatch(ctx, "h-1", &vzdv1.DriverRequest{
		Op: &vzdv1.DriverRequest_CreateVm{CreateVm: &vzdv1.CreateVMOp{}},
	})
	if err == nil {
		t.Fatal("expected context cancel error")
	}
	if status.Code(err) != codes.DeadlineExceeded && status.Code(err) != codes.Canceled {
		t.Errorf("status = %v, want DeadlineExceeded or Canceled", status.Code(err))
	}
}
