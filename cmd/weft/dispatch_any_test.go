package main

// dispatch_any_test.go covers the v0.4.52 cutover layer :
// dispatchAny picks AttachDrivers when a session exists, falls
// back to AgentDispatch otherwise, surfaces Unavailable when
// neither transport has a session.

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	weft "github.com/openweft/weft"
	agentv1 "github.com/openweft/weft-proto/agentv1"
	weftv1 "github.com/openweft/weft-proto"
)

// TestDispatchAny_NoTransport pins that dispatchAny on a server
// with neither transport set returns Unavailable (no nil-deref).
func TestDispatchAny_NoTransport(t *testing.T) {
	s := &weftServer{}
	_, err := s.dispatchAny(context.Background(), "host-x", &weftv1.DriverRequest{
		Op: &weftv1.DriverRequest_StartVm{StartVm: &weftv1.StartVMOp{Name: "vm"}},
	})
	if err == nil {
		t.Fatal("expected Unavailable, got nil")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("status=%v, want Unavailable", got)
	}
}

// TestDispatchAny_NilOpRejected pins that a nil DriverRequest is
// caught at the helper boundary, not at a transport.
func TestDispatchAny_NilOpRejected(t *testing.T) {
	s := &weftServer{}
	_, err := s.dispatchAny(context.Background(), "host-x", nil)
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument on nil op, got %v", err)
	}
}

// TestDispatchAny_FallsBackToAgentDispatch pins the default path :
// no AttachDrivers session exists, dispatchAny goes via
// AgentDispatch.Connect. Uses bufconn for the agent stream and a
// fake handler that echoes the request_id back.
func TestDispatchAny_FallsBackToAgentDispatch(t *testing.T) {
	disp := newAgentDispatchServer()
	disp.keepaliveInterval = 0 // disable keepalive for the test
	disp.livenessTimeout = 0   // disable liveness sweep too

	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	weftv1.RegisterAgentDispatchServer(srv, disp)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dial := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Run a tiny fake agent that opens the stream + echoes Reply
	// frames carrying the request_id verbatim.
	stream, err := weftv1.NewAgentDispatchClient(conn).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&weftv1.AgentMessage{
		Body: &weftv1.AgentMessage_Hello{Hello: &weftv1.AgentHello{HostUuid: "host-legacy", AgentVersion: "test"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Drain the HelloAck the server sends before any Dispatch lands.
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("recv hello ack: %v", err)
	}
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if req := msg.GetRequest(); req != nil {
				_ = stream.Send(&weftv1.AgentMessage{Body: &weftv1.AgentMessage_Reply{Reply: &weftv1.DriverReply{
					RequestId: req.RequestId,
				}}})
			}
		}
	}()

	// Wait briefly for register to land in the map (Connect goroutine).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && disp.SessionCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if disp.SessionCount() == 0 {
		t.Fatal("AgentDispatch session never registered")
	}

	// dispatchAny on a server with ONLY the dispatch field set →
	// should fall through to agent_dispatch transport.
	s := &weftServer{dispatch: disp}
	if got := s.dispatchTransportLabel("host-legacy"); got != "agent_dispatch" {
		t.Errorf("transport label = %q, want agent_dispatch", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := s.dispatchAny(ctx, "host-legacy", &weftv1.DriverRequest{
		Op: &weftv1.DriverRequest_StartVm{StartVm: &weftv1.StartVMOp{Project: "p", Name: "vm"}},
	})
	if err != nil {
		t.Fatalf("dispatchAny: %v", err)
	}
	if reply == nil {
		t.Fatal("nil reply")
	}

	_ = stream.CloseSend()
	select {
	case <-agentDone:
	case <-time.After(2 * time.Second):
		// Stream cleanup is best-effort ; the test goal already verified.
	}
}

// TestDispatchAny_PrefersAttachDriversWhenSessionExists pins the
// happy-path cutover : a host with an AttachDrivers session takes
// the new transport ; the opaque DriverRequest round-trip works.
func TestDispatchAny_PrefersAttachDriversWhenSessionExists(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	attachSrv := &agentControlPlaneServer{adp: adp}

	// Spin up the AttachDrivers gRPC service with the Dev-caller
	// interceptor + a fake agent that echoes DriverRequest payloads
	// straight back (no real driver dispatch needed).
	lis := bufconn.Listen(1 << 16)
	gsrv := grpc.NewServer(
		grpc.StreamInterceptor(func(srvObj any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			ctx := weft.WithCaller(ss.Context(), &weft.Caller{Dev: true})
			return handler(srvObj, &devCallerStream{ServerStream: ss, ctx: ctx})
		}),
	)
	agentv1.RegisterAgentControlPlaneServer(gsrv, attachSrv)
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)

	dial := func(context.Context, string) (net.Conn, error) { return lis.Dial() }
	conn, err := grpc.NewClient("passthrough://bufconn",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	agent := &fakeAgent{
		hostUUID:  "host-attach",
		driverSet: []string{"weft"},
		handle: func(_ context.Context, c *agentv1.DriverDispatchCall) *agentv1.DriverDispatchResult {
			// Decode the embedded DriverRequest, build a matching
			// DriverReply, re-encode as the result Payload.
			var req weftv1.DriverRequest
			if err := proto.Unmarshal(c.Payload, &req); err != nil {
				return &agentv1.DriverDispatchResult{CallId: c.CallId, ErrorMessage: "unmarshal: " + err.Error()}
			}
			// Trivial echo : success for everything.
			reply := &weftv1.DriverReply{}
			b, _ := proto.Marshal(reply)
			return &agentv1.DriverDispatchResult{CallId: c.CallId, Payload: b}
		},
	}
	client := agentv1.NewAgentControlPlaneClient(conn)
	agentCtx, agentCancel := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		_ = agent.run(agentCtx, t, client)
	}()
	t.Cleanup(func() {
		agentCancel()
		<-agentDone
	})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && attachSrv.AttachSessionCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if attachSrv.AttachSessionCount() == 0 {
		t.Fatal("AttachDrivers session never registered")
	}

	// dispatchAny on a server with ONLY the attach field set →
	// should pick the attach transport.
	s := &weftServer{attach: attachSrv}
	if got := s.dispatchTransportLabel("host-attach"); got != "attach" {
		t.Errorf("transport label = %q, want attach", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reply, err := s.dispatchAny(ctx, "host-attach", &weftv1.DriverRequest{
		Op: &weftv1.DriverRequest_StopVm{StopVm: &weftv1.StopVMOp{Name: "vm-1"}},
	})
	if err != nil {
		t.Fatalf("dispatchAny: %v", err)
	}
	if reply == nil {
		t.Fatal("nil reply")
	}
}

// TestDispatchAny_AttachUnavailableFallback pins the policy : when
// the AttachDrivers session is the ONLY one and the lookup misses
// (host has no session anywhere), we get Unavailable from the
// fallback agent_dispatch path (which itself has no session).
func TestDispatchAny_AttachUnavailableFallback(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	attachSrv := &agentControlPlaneServer{adp: adp}
	disp := newAgentDispatchServer()
	disp.keepaliveInterval = 0
	disp.livenessTimeout = 0

	s := &weftServer{attach: attachSrv, dispatch: disp}
	_, err := s.dispatchAny(context.Background(), "no-such-host", &weftv1.DriverRequest{
		Op: &weftv1.DriverRequest_StartVm{StartVm: &weftv1.StartVMOp{Name: "v"}},
	})
	if err == nil {
		t.Fatal("expected Unavailable for missing host")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Errorf("status=%v, want Unavailable (err=%v)", got, err)
	}
	if !strings.Contains(err.Error(), "no-such-host") {
		t.Errorf("error should mention the host UUID, got %v", err)
	}
	_ = io.EOF // silence unused import if errors evolves
	_ = errors.New
}
