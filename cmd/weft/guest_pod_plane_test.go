package main

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// TestGuestPodPlane_AttachHelloAck wires the handler through a real
// gRPC stream via bufconn and asserts the Hello → HelloAck protocol :
// the client sends a Hello, the server responds with an Ack carrying
// the pod_id, then the client drops cleanly.
func TestGuestPodPlane_AttachHelloAck(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	guestv1.RegisterGuestPodPlaneServer(srv, &guestPodPlaneServer{})
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

	client := guestv1.NewGuestPodPlaneClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Attach(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Send Hello.
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_Hello{
			Hello: &guestv1.GuestHello{
				PodId:       "pod-1",
				InitVersion: "test-1.0",
				Kernel:      "6.12.0-test",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Recv Ack.
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv ack: %v", err)
	}
	ack, ok := resp.Body.(*guestv1.GuestFrame_HelloAck)
	if !ok {
		t.Fatalf("expected HelloAck, got %T", resp.Body)
	}
	if ack.HelloAck.Spec == nil || ack.HelloAck.Spec.PodId != "pod-1" {
		t.Errorf("ack.Spec.PodId = %v, want pod-1", ack.HelloAck.Spec)
	}

	// Clean close.
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Errorf("expected EOF after close, got %v", err)
	}
}

// TestGuestPodPlane_RejectsNonHelloFirstFrame pins the wire contract :
// the first frame MUST be Hello ; anything else closes the stream
// with InvalidArgument.
func TestGuestPodPlane_RejectsNonHelloFirstFrame(t *testing.T) {
	lis := bufconn.Listen(1 << 16)
	srv := grpc.NewServer()
	guestv1.RegisterGuestPodPlaneServer(srv, &guestPodPlaneServer{})
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

	client := guestv1.NewGuestPodPlaneClient(conn)
	stream, err := client.Attach(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Send PodStatus before Hello : protocol violation.
	if err := stream.Send(&guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_PodStatus{PodStatus: &guestv1.PodStatus{PodId: "x"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Errorf("expected error on protocol violation, got nil")
	}
}
