package main

// guest_pod_plane.go implements the GuestPodPlane gRPC service
// (weft-proto guestv1, defined in guest.proto). This is the bidi
// stream weft-init (PID 1 inside an openweft micro-VM) uses to
// report pod / container events + receive control requests.
//
// Transport : the proto's comment describes the production transport
// as AF_VSOCK between guest and host. The gRPC server here is
// transport-agnostic — it sits on whatever socket the agent's
// listener was bound to (Unix today ; AF_VSOCK in a future commit
// that binds a per-VM vsock listener). The handler logic is the
// same regardless of the underlying socket.
//
// Today's implementation :
//   1. Accept the first GuestHello frame (mandatory per the proto).
//   2. Send a GuestHelloAck carrying the PodSpec the agent has on
//      record for the announced pod_id (or an empty ack when the
//      pod isn't known — operator hasn't created it yet ; the guest
//      stays connected and the agent can push state via future
//      ControlRequest frames).
//   3. Drain subsequent frames (PodStatus / ContainerEvent / LogChunk
//      / ControlResponse) ; log them at debug level. Pod-side state
//      reconciliation (storing PodStatus into etcd, fanning log
//      chunks to the weft-doctor pipeline) is the operator's
//      follow-up — the framing protocol is exercised end-to-end here.

import (
	"errors"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

type guestPodPlaneServer struct {
	guestv1.UnimplementedGuestPodPlaneServer
}

// Attach reads the mandatory GuestHello frame, replies with a
// GuestHelloAck, then drains. Return path on errors :
//   - first frame not a GuestHello : InvalidArgument
//   - send Ack fails : Internal (transport dropped)
//   - subsequent recv error or EOF : nil (clean shutdown)
func (s *guestPodPlaneServer) Attach(stream guestv1.GuestPodPlane_AttachServer) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return status.Errorf(codes.Canceled, "guest pod plane: recv hello: %v", err)
	}
	hello, ok := first.Body.(*guestv1.GuestFrame_Hello)
	if !ok || hello.Hello == nil {
		return status.Error(codes.InvalidArgument, "first frame must be GuestHello")
	}
	logger.Printf("GuestPodPlane attached : pod=%s init=%s kernel=%s",
		hello.Hello.PodId, hello.Hello.InitVersion, hello.Hello.Kernel)

	// Send HelloAck. Empty PodSpec for now ; operator-side pod
	// registration lands the spec into the agent's pod registry,
	// and the next iteration of this handler reads from that
	// registry to populate the ack.
	ack := &guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_HelloAck{
			HelloAck: &guestv1.GuestHelloAck{
				Spec: &guestv1.PodSpec{PodId: hello.Hello.PodId},
			},
		},
	}
	if err := stream.Send(ack); err != nil {
		return status.Errorf(codes.Internal, "guest pod plane: send ack: %v", err)
	}

	// Drain : log frames at debug level. Production logging for
	// LogChunk should fan into weft-doctor's NATS subject ; the
	// initial impl keeps the wire test green without that fan-out.
	for {
		frame, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return nil // any other error → clean close (guest dropped)
		}
		switch b := frame.Body.(type) {
		case *guestv1.GuestFrame_PodStatus:
			logger.Printf("guest %s : pod status uptime_ms=%d containers=%d",
				hello.Hello.PodId, b.PodStatus.UptimeMs, len(b.PodStatus.Containers))
		case *guestv1.GuestFrame_CtrEvent:
			st := ""
			if b.CtrEvent.Status != nil {
				st = b.CtrEvent.Status.State
			}
			logger.Printf("guest %s : ctr event id=%s kind=%s state=%s",
				hello.Hello.PodId, b.CtrEvent.Id, b.CtrEvent.Kind, st)
		case *guestv1.GuestFrame_Log:
			// Log chunks intentionally non-noisy : the fan-out to
			// weft-doctor / NATS is the follow-up. Don't print every
			// byte to the agent's stderr.
		case *guestv1.GuestFrame_ControlResp:
			logger.Printf("guest %s : control resp call_id=%d err=%q",
				hello.Hello.PodId, b.ControlResp.CallId, b.ControlResp.Error)
		}
	}
}
