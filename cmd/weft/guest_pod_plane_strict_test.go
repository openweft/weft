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
	cids  map[string]uint32
	specs map[string]*guestv1.PodSpec
}

func (f *fakeAdapter) PodCID(podID string) (uint32, bool) {
	c, ok := f.cids[podID]
	return c, ok
}

func (f *fakeAdapter) PodSpec(podID string) (*guestv1.PodSpec, bool) {
	s, ok := f.specs[podID]
	return s, ok
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

// helloDenied sends a Hello for podID from peer addr and asserts the
// stream ends in PermissionDenied with no frame (no spec) delivered.
func helloDenied(t *testing.T, h *strictPodPlaneServer, addr fakeVsockAddr, podID string) {
	t.Helper()
	client := makeServerWithPeer(t, h, addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{PodId: podID}},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if resp != nil {
		t.Fatalf("a refused Hello received a frame: %T", resp.Body)
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("status=%v, want PermissionDenied (err=%v)", got, err)
	}
}

// An unbound pod id gets no spec, even from a valid guest CID.
func TestGuestPodPlane_UnknownPod_Refused(t *testing.T) {
	adp := &fakeAdapter{
		cids:  map[string]uint32{},
		specs: map[string]*guestv1.PodSpec{"pod-1": {PodId: "pod-1"}},
	}
	helloDenied(t, &strictPodPlaneServer{adp: adp}, fakeVsockAddr{cid: 4242}, "pod-1")
	if len(adp.cids) != 0 {
		t.Error("a refused Hello must not bind anything")
	}
}

// A guest announcing another pod's id gets no spec, and the real
// pod's binding is untouched.
func TestGuestPodPlane_OtherPodsID_NoSpec(t *testing.T) {
	adp := &fakeAdapter{
		cids:  map[string]uint32{"victim": 4242, "mine": 7777},
		specs: map[string]*guestv1.PodSpec{"victim": {PodId: "victim"}},
	}
	helloDenied(t, &strictPodPlaneServer{adp: adp}, fakeVsockAddr{cid: 7777}, "victim")
	if adp.cids["victim"] != 4242 {
		t.Error("binding changed")
	}
}

// Reserved CIDs (0 among them) are refused before any lookup.
func TestGuestPodPlane_ReservedCIDs_Refused(t *testing.T) {
	for _, cid := range []uint32{0, 1, 2, 0xffffffff} {
		adp := &fakeAdapter{
			cids:  map[string]uint32{"pod-1": cid},
			specs: map[string]*guestv1.PodSpec{"pod-1": {PodId: "pod-1"}},
		}
		helloDenied(t, &strictPodPlaneServer{adp: adp}, fakeVsockAddr{cid: cid}, "pod-1")
	}
}

// A call with no peer information is refused, not waved through.
func TestGuestPodPlane_NoPeer_Refused(t *testing.T) {
	h := &strictPodPlaneServer{adp: &fakeAdapter{
		cids:  map[string]uint32{"pod-1": 4242},
		specs: map[string]*guestv1.PodSpec{"pod-1": {PodId: "pod-1"}},
	}}
	stream := &noPeerStream{}
	err := h.Attach(stream)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("err=%v, want PermissionDenied", err)
	}
	if stream.sent != 0 {
		t.Fatal("a frame was sent")
	}
}

// A peer record with no address, or a non-vsock address, is refused
// even when the pod is bound; nothing is sent either way.
func TestGuestPodPlane_PeerWithoutVsockIdentity_Refused(t *testing.T) {
	h := &strictPodPlaneServer{adp: &fakeAdapter{
		cids:  map[string]uint32{"pod-1": 4242},
		specs: map[string]*guestv1.PodSpec{"pod-1": {PodId: "pod-1"}},
	}}
	for name, p := range map[string]*peer.Peer{
		"nil addr": {},
		"tcp addr": {Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}},
	} {
		stream := &noPeerStream{ctx: peer.NewContext(context.Background(), p)}
		if err := h.Attach(stream); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: err=%v, want PermissionDenied", name, err)
		}
		if stream.sent != 0 {
			t.Errorf("%s: a frame was sent", name)
		}
	}
}

// A bound pod called from its own CID is still refused when the
// guest's reported CID disagrees with the peer's.
func TestGuestPodPlane_BoundPodReportedMismatch_Refused(t *testing.T) {
	adp := &fakeAdapter{
		cids:  map[string]uint32{"pod-1": 4242},
		specs: map[string]*guestv1.PodSpec{"pod-1": {PodId: "pod-1"}},
	}
	h := &strictPodPlaneServer{adp: adp}
	client := makeServerWithPeer(t, h, fakeVsockAddr{cid: 4242})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{PodId: "pod-1", ReportedCid: 9999}},
	}); err != nil {
		t.Fatal(err)
	}
	if resp, err := stream.Recv(); resp != nil || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("resp=%v err=%v, want PermissionDenied and no frame", resp, err)
	}
}

type noPeerStream struct {
	grpc.ServerStream
	sent int
	ctx  context.Context
}

func (s *noPeerStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *noPeerStream) Send(*guestv1.GuestFrame) error {
	s.sent++
	return nil
}
func (s *noPeerStream) Recv() (*guestv1.GuestFrame, error) { return nil, io.EOF }

// Only the guest services are reachable over the vsock server.
func TestNewGuestServer_ExposesGuestServicesOnly(t *testing.T) {
	srv := newGuestServer(&fakeAdapter{})
	got := srv.GetServiceInfo()
	if len(got) != 1 {
		t.Fatalf("services = %v, want only GuestPodPlane", got)
	}
	if _, ok := got["weft.guest.v1.GuestPodPlane"]; !ok {
		t.Fatalf("services = %v, want GuestPodPlane", got)
	}
}

// TestGuestPodPlane_HelloAckCarriesPodSpec is the contract pin for
// the operator-published-spec path : when the adapter has a PodSpec
// recorded for the announced pod_id, the HelloAck frame echoes it
// back to the guest. Without that the in-guest container reconciler
// never gets its desired state.
func TestGuestPodPlane_HelloAckCarriesPodSpec(t *testing.T) {
	want := &guestv1.PodSpec{
		PodId: "pod-1",
		Containers: []*guestv1.Container{
			{Id: "c1", RootfsTag: "rootfs-c1", Command: []string{"/bin/sleep", "infinity"}},
		},
	}
	adp := &fakeAdapter{
		cids:  map[string]uint32{"pod-1": 4242},
		specs: map[string]*guestv1.PodSpec{"pod-1": want},
	}
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
	ack, ok := resp.Body.(*guestv1.GuestFrame_HelloAck)
	if !ok {
		t.Fatalf("got %T, want HelloAck", resp.Body)
	}
	got := ack.HelloAck.GetSpec()
	if got == nil || got.PodId != "pod-1" || len(got.Containers) != 1 || got.Containers[0].Id != "c1" {
		t.Errorf("HelloAck spec mismatch : %+v", got)
	}
}

// TestGuestPodPlane_ReportedCIDMismatchRejected pins the
// guest-reported vs. peer-observed cross-check. A guest that lies
// about its CID (reported_cid != peer.CID()) is refused before any
// autoregister or strict-when-known logic runs.
func TestGuestPodPlane_ReportedCIDMismatchRejected(t *testing.T) {
	adp := &fakeAdapter{cids: map[string]uint32{}}
	h := &strictPodPlaneServer{adp: adp}
	client := makeServerWithPeer(t, h, fakeVsockAddr{cid: 4242})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{Hello: &guestv1.GuestHello{
			PodId:       "spoofer",
			ReportedCid: 9999, // claims a different CID than peer.CID()=4242
		}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected PermissionDenied on reported/peer mismatch")
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("status=%v, want PermissionDenied (err=%v)", got, err)
	}
	// And no autoregister should have happened.
	if _, ok := adp.cids["spoofer"]; ok {
		t.Error("a rejected Hello must not stamp the registry")
	}
}

// A nil registry cannot vouch for anyone : refuse.
func TestGuestPodPlane_NilAdapter_Refused(t *testing.T) {
	helloDenied(t, &strictPodPlaneServer{adp: nil}, fakeVsockAddr{cid: 4242}, "anything")
}
