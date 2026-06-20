package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	weft "github.com/openweft/weft"
	agentv1 "github.com/openweft/weft-proto/agentv1"
)

// TestAgentControlPlane_RegisterAgent covers the lifecycle RPC :
// translate HostRegistration → RegisterHostSpec, return the assigned
// UUID, idempotent re-register.
func TestAgentControlPlane_RegisterAgent(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	s := &agentControlPlaneServer{adp: adp}

	resp, err := s.RegisterAgent(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.RegisterAgentRequest{
		Registration: &agentv1.HostRegistration{
			Uuid:         "host-uuid-1",
			Hostname:     "h1",
			Az:           "dc1",
			Rack:         "r1",
			Hypervisor:   "qemu",
			Architecture: "arm64",
			Properties:   map[string]string{"tier": "edge"},
		},
	})
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	if resp.AssignedUuid != "host-uuid-1" {
		t.Errorf("assigned = %q, want host-uuid-1", resp.AssignedUuid)
	}

	// Re-register with same UUID + new placement field : idempotent +
	// new value wins via the existing RegisterHost semantics.
	resp2, err := s.RegisterAgent(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.RegisterAgentRequest{
		Registration: &agentv1.HostRegistration{
			Uuid: "host-uuid-1", Hostname: "h1",
			Az: "dc2", Rack: "r9", Hypervisor: "qemu", Architecture: "arm64",
		},
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if resp2.AssignedUuid != "host-uuid-1" {
		t.Errorf("re-register uuid changed : got %q", resp2.AssignedUuid)
	}
	h, ok := adp.HostByUUID("host-uuid-1")
	if !ok || h.AZ != "dc2" || h.Rack != "r9" {
		t.Errorf("re-register didn't update AZ/Rack : %+v", h)
	}
}

// TestAgentControlPlane_RegisterAgent_EmptyRequestRejected pins the
// InvalidArgument contract.
func TestAgentControlPlane_RegisterAgent_EmptyRequestRejected(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	s := &agentControlPlaneServer{adp: adp}
	_, err := s.RegisterAgent(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.RegisterAgentRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}

// TestAgentControlPlane_Heartbeat covers the heartbeat RPC against
// a registered host + the NotFound case.
func TestAgentControlPlane_Heartbeat(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)
	s := &agentControlPlaneServer{adp: adp}
	// Register first.
	if _, err := s.RegisterAgent(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.RegisterAgentRequest{
		Registration: &agentv1.HostRegistration{Uuid: "h", Hostname: "h", Hypervisor: "qemu", Architecture: "arm64"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.HeartbeatRequest{HostUuid: "h"}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	// Unknown host : NotFound.
	_, err := s.Heartbeat(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.HeartbeatRequest{HostUuid: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown host : got %v, want NotFound", err)
	}
	// Empty arg : InvalidArgument.
	_, err = s.Heartbeat(weft.WithCaller(context.Background(), &weft.Caller{Dev: true}), &agentv1.HeartbeatRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty arg : got %v, want InvalidArgument", err)
	}
}

// TestAgentControlPlane_AttachDrivers_CounterIncrementsOnClientEOF
// drives the AttachDrivers bidi stream over bufconn : send a valid
// Init frame, close the client half, and assert that the
// `weft_attach_drivers_calls_total{result="client_eof"}` counter
// incremented exactly once. The same stream also produces one
// `result="opened"` sample on Init-accept ; we assert that too as a
// shape check on the v0.4.49 observability seam.
//
// Auth is bypassed by a server-side stream interceptor that injects
// a Dev caller — same shortcut the in-process unary tests above use
// via weft.WithCaller, just lifted to the stream interceptor seam
// since the bidi stream's ctx is rooted at the gRPC layer, not in
// the test goroutine.
func TestAgentControlPlane_AttachDrivers_CounterIncrementsOnClientEOF(t *testing.T) {
	adp := weft.NewWithStorage(t.TempDir(), nil).(*weft.Adapter)

	// Fresh isolated registry so we don't fight tests that run in
	// parallel against prometheus.DefaultRegisterer ; mutates the
	// package-level attachDriversCalls handle for the duration of
	// the test, restored on cleanup so neighbour tests see a clean
	// slate.
	prevCounter := attachDriversCalls
	t.Cleanup(func() { attachDriversCalls = prevCounter })
	reg := prometheus.NewRegistry()
	counter, err := newAttachDriversMetrics(reg)
	if err != nil {
		t.Fatalf("newAttachDriversMetrics: %v", err)
	}

	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer(
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			ctx := weft.WithCaller(ss.Context(), &weft.Caller{Dev: true})
			return handler(srv, &devCallerStream{ServerStream: ss, ctx: ctx})
		}),
	)
	agentv1.RegisterAgentControlPlaneServer(srv, &agentControlPlaneServer{adp: adp})
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

	client := agentv1.NewAgentControlPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.AttachDrivers(ctx)
	if err != nil {
		t.Fatalf("AttachDrivers: %v", err)
	}

	if err := stream.Send(&agentv1.AttachDriversFrame{
		Body: &agentv1.AttachDriversFrame_Init{
			Init: &agentv1.AttachDriversInit{
				HostUuid:    "host-attach-1",
				DriverKinds: []string{"hypervisor", "network"},
			},
		},
	}); err != nil {
		t.Fatalf("send init: %v", err)
	}

	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	// Drain the server side. AttachDrivers returns nil on EOF, which
	// the gRPC transport surfaces to the client as io.EOF on Recv.
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("expected EOF after server returns nil, got %v", err)
	}

	_ = counter // counter handle retained for symmetry with the other metrics tests ; reads go through reg.Gather below.

	// Server-side counter increment happens INSIDE the handler return,
	// so by the time client.Recv sees io.EOF the sample has landed.
	// Poll briefly anyway — bufconn + gRPC stream teardown can race
	// the deferred Inc in pathological scheduler conditions.
	if got := waitForCounter(t, reg, "client_eof", 1.0); got != 1 {
		t.Errorf("client_eof counter = %v, want 1", got)
	}
	if got := counterValueByLabels(t, reg, "weft_attach_drivers_calls_total", map[string]string{"result": "opened"}); got != 1 {
		t.Errorf("opened counter = %v, want 1", got)
	}
	// init_error / error / server_eof should stay at 0 for this path.
	for _, label := range []string{"init_error", "error", "server_eof"} {
		if got := counterValueByLabels(t, reg, "weft_attach_drivers_calls_total", map[string]string{"result": label}); got != 0 {
			t.Errorf("%s counter = %v, want 0", label, got)
		}
	}
}

// devCallerStream wraps a server-side ServerStream so the inner
// handler sees a context carrying a Dev caller — same pattern the
// production auth interceptor uses, lifted into the test rig so
// RequireAdmin passes inside the AttachDrivers handler.
type devCallerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (d *devCallerStream) Context() context.Context { return d.ctx }

// waitForCounter polls the registry for up to ~1s until the
// weft_attach_drivers_calls_total{result=<label>} sample reaches
// `want` or the timeout fires. Guards against the rare race where
// the gRPC stream returns to the client before the server-side
// handler defers have run.
func waitForCounter(t *testing.T, reg *prometheus.Registry, label string, want float64) float64 {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if got := counterValueByLabels(t, reg, "weft_attach_drivers_calls_total", map[string]string{"result": label}); got >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return counterValueByLabels(t, reg, "weft_attach_drivers_calls_total", map[string]string{"result": label})
}
