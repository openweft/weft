package main

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// fakeVsockAddr is a net.Addr that also implements the CID() uint32
// method the production handler probes via a type assertion. Lets
// the strict-CID tests drive the handler over bufconn while pretending
// the peer came in over AF_VSOCK.
type fakeVsockAddr struct{ cid uint32 }

func (a fakeVsockAddr) Network() string { return "vsock" }
func (a fakeVsockAddr) String() string  { return "vsock:" + strconv.FormatUint(uint64(a.cid), 10) }
func (a fakeVsockAddr) CID() uint32     { return a.cid }

// fakeAdapter satisfies just the slice of weft.VZAdapter the
// GuestPodPlane handler actually calls (PodCID). Everything else is
// safely left at zero-value because guest_pod_plane.go's strict path
// only invokes PodCID.
type fakeAdapter struct {
	cids map[string]uint32
}

func (f *fakeAdapter) PodCID(podID string) (uint32, bool) {
	c, ok := f.cids[podID]
	return c, ok
}

// makeServerWithPeer launches a bufconn-served gRPC stack that
// stamps the supplied vsockAddr onto every incoming RPC's peer
// info, so the GuestPodPlane handler sees the same shape it would
// over a real AF_VSOCK listener.
func makeServerWithPeer(t *testing.T, h *strictPodPlaneServer, addr fakeVsockAddr) guestv1.GuestPodPlaneClient {
	t.Helper()
	lis := bufconn.Listen(1 << 16)
	interceptor := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := peer.NewContext(ss.Context(), &peer.Peer{Addr: addr})
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
	srv := grpc.NewServer(grpc.StreamInterceptor(interceptor))
	guestv1.RegisterGuestPodPlaneServer(srv, h)
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
	t.Cleanup(func() { conn.Close() })
	return guestv1.NewGuestPodPlaneClient(conn)
}

// wrappedStream is the minimal grpc.ServerStream wrapper needed to
// swap the context the handler observes — peer.NewContext on the
// inbound stream is the cleanest way to inject a fake peer without
// touching the listener's transport.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// strictPodPlaneServer mirrors the production server but lets the
// test wire a fakeAdapter without dragging the full weft.Adapter
// constructor into the test binary.
type strictPodPlaneServer = guestPodPlaneServer

// TestGuestPodPlane_StrictCIDMismatch_Rejects pins the central
// security invariant : when the agent has a known CID for a pod_id,
// a Hello announcing that pod_id from a *different* CID is refused
// with PermissionDenied.
func TestGuestPodPlane_StrictCIDMismatch_Rejects(t *testing.T) {
	adp := &fakeAdapter{cids: map[string]uint32{"pod-1": 4242}}
	h := &strictPodPlaneServer{adp: adp}
	client := makeServerWithPeer(t, h, fakeVsockAddr{cid: 9999}) // wrong CID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{PodId: "pod-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected PermissionDenied, got nil")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("status=%v, want PermissionDenied (err=%v)", got, err)
	}
}

// TestGuestPodPlane_StrictCIDMatch_Accepts is the happy path : the
// peer's CID matches the recorded one ; the handler proceeds to the
// HelloAck.
func TestGuestPodPlane_StrictCIDMatch_Accepts(t *testing.T) {
	adp := &fakeAdapter{cids: map[string]uint32{"pod-1": 4242}}
	h := &strictPodPlaneServer{adp: adp}
	client := makeServerWithPeer(t, h, fakeVsockAddr{cid: 4242})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{PodId: "pod-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if _, ok := resp.Body.(*guestv1.GuestFrame_HelloAck); !ok {
		t.Errorf("got %T, want HelloAck", resp.Body)
	}
	_ = stream.CloseSend()
	if _, err := stream.Recv(); err != nil && err != io.EOF {
		t.Errorf("expected EOF after close, got %v", err)
	}
}

// TestGuestPodPlane_UnknownPod_FallsBackToCIDGuard exercises the
// "permissive when unknown" half of the contract : the pod_id isn't
// in the registry, the peer's CID is a valid non-reserved guest CID,
// the handler proceeds. This is what protects legacy VMs registered
// before the allocator landed (VsockCID==0).
func TestGuestPodPlane_UnknownPod_FallsBackToCIDGuard(t *testing.T) {
	adp := &fakeAdapter{cids: map[string]uint32{}} // empty
	h := &strictPodPlaneServer{adp: adp}
	client := makeServerWithPeer(t, h, fakeVsockAddr{cid: 4242}) // valid guest CID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{PodId: "legacy-pod"}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if _, ok := resp.Body.(*guestv1.GuestFrame_HelloAck); !ok {
		t.Errorf("got %T, want HelloAck for unknown pod (permissive path)", resp.Body)
	}
}

// TestGuestPodPlane_NilAdapter_NoStrictCheck pins the explicit
// nil-adapter path : without an adapter the strict check is skipped,
// only the generic non-reserved guard runs. The fake peer carries a
// guest CID, so the call should proceed.
func TestGuestPodPlane_NilAdapter_NoStrictCheck(t *testing.T) {
	h := &strictPodPlaneServer{adp: nil}
	client := makeServerWithPeer(t, h, fakeVsockAddr{cid: 4242})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{PodId: "anything"}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	if _, ok := resp.Body.(*guestv1.GuestFrame_HelloAck); !ok {
		t.Errorf("got %T, want HelloAck (nil-adapter path)", resp.Body)
	}
}
