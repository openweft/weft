package main

// agent_control_plane_dispatch_test.go exercises the v0.4.50
// AttachDrivers dispatch path : real DriverDispatchCall + DriverDispatchResult
// round-trip with call_id correlation, session register/deregister
// lifecycle, no-session Unavailable error.
//
// Pattern lifted from agent_control_plane_test.go : bufconn-based
// gRPC server, devCallerStream interceptor injecting Dev caller so
// RequireAdmin passes.

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	weft "github.com/openweft/weft"
	agentv1 "github.com/openweft/weft-proto/agentv1"
)

// fakeAgent runs a client-side AttachDrivers session that auto-echoes
// every DriverDispatchCall it receives as a DriverDispatchResult with
// the same call_id. The handler argument lets a test customise the
// reply (e.g. populate ErrorMessage, change Payload). Returns once
// the server closes the stream or ctx fires.
type fakeAgent struct {
	hostUUID  string
	driverSet []string
	// handle receives the call + the agent's run context so a test
	// that wants to NEVER reply (to exercise the drain path) can
	// block on <-ctx.Done() and bail cleanly when the test tears
	// the agent down. Return nil to skip the reply entirely.
	handle func(ctx context.Context, c *agentv1.DriverDispatchCall) *agentv1.DriverDispatchResult
}

func (a *fakeAgent) run(ctx context.Context, t *testing.T, client agentv1.AgentControlPlaneClient) error {
	t.Helper()
	stream, err := client.AttachDrivers(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.AttachDriversFrame{
		Body: &agentv1.AttachDriversFrame_Init{
			Init: &agentv1.AttachDriversInit{
				HostUuid:    a.hostUUID,
				DriverKinds: a.driverSet,
			},
		},
	}); err != nil {
		return err
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			// Cancelled stream looks like an Unavailable on bufconn
			// after the server returns from the handler.
			if status.Code(err) == codes.Canceled || status.Code(err) == codes.Unavailable {
				return nil
			}
			return err
		}
		call := frame.GetDispatch()
		if call == nil {
			continue
		}
		// Handle in a goroutine so the recv loop can still observe
		// ctx cancellation while a slow / never-replying handle is
		// running. Without this, a handle that blocks forever
		// (the drain-test pattern) would prevent fakeAgent.run from
		// exiting when the test cancels the context.
		go func(call *agentv1.DriverDispatchCall) {
			reply := a.handle(ctx, call)
			if reply == nil {
				// nil = "don't reply at all". The Dispatch caller's
				// pending entry stays open ; the only way it
				// unblocks is via the session drain or its own ctx
				// cancellation. Used by the drain test.
				return
			}
			if reply.CallId == 0 {
				reply.CallId = call.CallId
			}
			_ = stream.Send(&agentv1.AttachDriversFrame{
				Body: &agentv1.AttachDriversFrame_Result{Result: reply},
			})
		}(call)
	}
}

// newDispatchTestServer spins up a bufconn-served AgentControlPlane
// + client + an attached fakeAgent. Returns the server (so tests can
// call Dispatch on it) + the client + a teardown that closes the
// fakeAgent's stream and waits for its goroutine to exit.
func newDispatchTestServer(t *testing.T, agent *fakeAgent) (*agentControlPlaneServer, func()) {
	t.Helper()
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	srv := &agentControlPlaneServer{adp: adp}

	lis := bufconn.Listen(1 << 16)
	grpcSrv := grpc.NewServer(
		grpc.StreamInterceptor(func(srvObj any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			ctx := weft.WithCaller(ss.Context(), &weft.Caller{Dev: true})
			return handler(srvObj, &devCallerStream{ServerStream: ss, ctx: ctx})
		}),
	)
	agentv1.RegisterAgentControlPlaneServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()

	dial := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := agentv1.NewAgentControlPlaneClient(conn)

	agentCtx, agentCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = agent.run(agentCtx, t, client)
	}()

	// Wait for the session to register before returning so tests can
	// call Dispatch immediately. 1s with a 5ms poll is conservative
	// for bufconn — the Init handshake usually settles in <10ms.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if srv.AttachSessionCount() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if srv.AttachSessionCount() == 0 {
		agentCancel()
		wg.Wait()
		grpcSrv.Stop()
		_ = conn.Close()
		t.Fatal("AttachDrivers session never registered")
	}

	teardown := func() {
		agentCancel()
		_ = conn.Close()
		grpcSrv.Stop()
		wg.Wait()
	}
	return srv, teardown
}

// TestAttachDispatch_RoundTrip pins the call_id correlation contract :
// Dispatch sends a Call, the agent echoes it back as a Result with the
// matching CallId, the caller gets the result.
func TestAttachDispatch_RoundTrip(t *testing.T) {
	agent := &fakeAgent{
		hostUUID:  "host-dispatch-1",
		driverSet: []string{"hypervisor"},
		handle: func(_ context.Context, c *agentv1.DriverDispatchCall) *agentv1.DriverDispatchResult {
			return &agentv1.DriverDispatchResult{
				CallId:  c.CallId,
				Payload: append([]byte("echoed:"), c.Payload...),
			}
		},
	}
	srv, teardown := newDispatchTestServer(t, agent)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := srv.Dispatch(ctx, "host-dispatch-1", &agentv1.DriverDispatchCall{
		DriverKind: "hypervisor",
		MethodName: "CreateVM",
		Payload:    []byte("vmspec"),
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result == nil {
		t.Fatal("Dispatch returned nil result")
	}
	if string(result.Payload) != "echoed:vmspec" {
		t.Errorf("Payload = %q, want %q", result.Payload, "echoed:vmspec")
	}
	if result.CallId == 0 {
		t.Errorf("CallId = 0, want a non-zero server-minted id")
	}
}

// TestAttachDispatch_NoSessionReturnsUnavailable pins the failure mode
// for hosts without an open stream : Dispatch returns Unavailable, not
// a nil result or a generic Internal.
func TestAttachDispatch_NoSessionReturnsUnavailable(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	srv := &agentControlPlaneServer{adp: adp}

	_, err := srv.Dispatch(context.Background(), "no-such-host", &agentv1.DriverDispatchCall{
		DriverKind: "hypervisor",
		MethodName: "ListVMs",
	})
	if err == nil {
		t.Fatal("expected Unavailable, got nil")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("status = %v, want Unavailable (err=%v)", got, err)
	}
}

// TestAttachDispatch_ConcurrentCallsGetDistinctIDs pins that
// server-minted call_ids are unique within a session even under
// concurrent Dispatch calls — the agent's reply-correlator depends
// on this.
func TestAttachDispatch_ConcurrentCallsGetDistinctIDs(t *testing.T) {
	// Track ids the fakeAgent saw : if Dispatch races and re-uses
	// an id, the agent's reply to the first call would resolve the
	// second caller's pending entry by accident.
	var seenMu sync.Mutex
	seen := make(map[uint64]int)
	agent := &fakeAgent{
		hostUUID:  "host-dispatch-2",
		driverSet: []string{"hypervisor"},
		handle: func(_ context.Context, c *agentv1.DriverDispatchCall) *agentv1.DriverDispatchResult {
			seenMu.Lock()
			seen[c.CallId]++
			seenMu.Unlock()
			return &agentv1.DriverDispatchResult{CallId: c.CallId}
		},
	}
	srv, teardown := newDispatchTestServer(t, agent)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := srv.Dispatch(ctx, "host-dispatch-2", &agentv1.DriverDispatchCall{
				DriverKind: "hypervisor",
				MethodName: "Heartbeat",
			})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Dispatch failed : %v", err)
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != N {
		t.Errorf("agent saw %d distinct call_ids, want %d (collisions = %v)", len(seen), N, seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("call_id %d delivered %d times, want 1", id, count)
		}
	}
}

// TestAttachDispatch_SessionDeregistersOnEOF pins the cleanup contract :
// when the agent closes its half of the stream, the server-side
// session leaves the registry within a short window so future
// Dispatch calls fail fast with Unavailable instead of hanging.
func TestAttachDispatch_SessionDeregistersOnEOF(t *testing.T) {
	agent := &fakeAgent{
		hostUUID:  "host-dispatch-3",
		driverSet: []string{"network"},
		handle: func(_ context.Context, c *agentv1.DriverDispatchCall) *agentv1.DriverDispatchResult {
			return &agentv1.DriverDispatchResult{CallId: c.CallId}
		},
	}
	srv, teardown := newDispatchTestServer(t, agent)
	if srv.AttachSessionCount() != 1 {
		teardown()
		t.Fatalf("session count before teardown = %d, want 1", srv.AttachSessionCount())
	}
	teardown()
	// Server-side handler returns after the client cancels ; the
	// defer pulls the session out of the map. Poll briefly to win
	// the race against the bufconn / gRPC cleanup goroutines.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if srv.AttachSessionCount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("session count after teardown = %d, want 0", srv.AttachSessionCount())
}

// TestAttachDispatch_DrainOnDisconnect pins that Dispatch callers
// blocked on a result get woken with an Aborted status when the
// agent session ends mid-flight — no goroutine leak, no caller
// hung forever.
func TestAttachDispatch_DrainOnDisconnect(t *testing.T) {
	// Build an agent whose handle never sends a reply (returns nil)
	// so the Dispatch caller blocks on its replyCh until we tear
	// down the session. The drain on session-end is what we're
	// asserting.
	agent := &fakeAgent{
		hostUUID:  "host-dispatch-4",
		driverSet: []string{"hypervisor"},
		handle: func(ctx context.Context, _ *agentv1.DriverDispatchCall) *agentv1.DriverDispatchResult {
			// Block until the agent's ctx fires (teardown). Returning
			// nil at the end signals "no reply" to fakeAgent.run.
			<-ctx.Done()
			return nil
		},
	}

	srv, teardown := newDispatchTestServer(t, agent)

	// Fire a Dispatch in the background ; it should block on
	// replyCh until teardown drains the session.
	type dispatchOut struct {
		result *agentv1.DriverDispatchResult
		err    error
	}
	outCh := make(chan dispatchOut, 1)
	go func() {
		result, err := srv.Dispatch(context.Background(), "host-dispatch-4",
			&agentv1.DriverDispatchCall{DriverKind: "hypervisor", MethodName: "Stop"})
		outCh <- dispatchOut{result: result, err: err}
	}()

	// Give the goroutine a moment to register the pending entry +
	// land in the receive on replyCh. Without this we could tear
	// down before pending.register ran and the drain would be empty.
	time.Sleep(50 * time.Millisecond)

	teardown()

	select {
	case out := <-outCh:
		// Drain delivers a synthetic Result (not an err), so out.err
		// should be nil and out.result.ErrorMessage carries the abort
		// reason.
		if out.err != nil {
			// Acceptable too — the gRPC layer can race the drain and
			// fire the ctx-cancelled / Aborted path. Either way the
			// Dispatch returned, that's what we're testing.
			return
		}
		if out.result == nil || out.result.ErrorMessage == "" {
			t.Errorf("dispatch drain : result=%+v err=%v ; want either non-nil err OR result.ErrorMessage != \"\"", out.result, out.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch did not unblock after session drain — goroutine leak")
	}
}
