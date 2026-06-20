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
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	guestv1 "github.com/openweft/weft-proto/guestv1"
)

// podCIDLookup is the slice of the adapter the GuestPodPlane handler
// actually depends on. Defined here (not lifted from weft.VZAdapter)
// so tests can stub it without dragging the full adapter surface in.
// The production handler is constructed with the real adapter ; tests
// inject a tiny fake that only fills PodCID + PodSpec + RegisterPodCID.
type podCIDLookup interface {
	PodCID(podID string) (uint32, bool)
	PodSpec(podID string) (*guestv1.PodSpec, bool)
	// RegisterPodCID stamps a new (pod_id, cid) entry in the
	// host's podCIDs registry. v0.4.51 wires the GuestPodPlane
	// Hello handler to autoregister the peer's actual CID when
	// the registry has no entry yet — closes the Apple-VZ readback
	// gap by trusting the guest-reported CID after kernel-level
	// peer.CID() agreement, and lets QEMU-backed VMs self-heal if
	// the pre-allocated CID drifted from the kernel-bound one.
	RegisterPodCID(podID string, cid uint32)
}

type guestPodPlaneServer struct {
	guestv1.UnimplementedGuestPodPlaneServer
	// adp is the adapter the handler queries for the announced
	// pod_id's expected AF_VSOCK CID. Optional in tests — when nil
	// (or when the adapter has no entry for the pod_id) the handler
	// falls back to the existing "any non-reserved CID" guard.
	adp podCIDLookup
	// allowNonGuestCallers bypasses the peer-CID guard for tests
	// that exercise the protocol over bufconn (not AF_VSOCK).
	// Production never sets this — it's strictly a test escape
	// hatch so the wire-level happy-path tests can run without
	// faking up an AF_VSOCK transport.
	allowNonGuestCallers bool
}

// IsGuestCID reports whether the AF_VSOCK CID is a real guest VM's
// id (not a kernel-reserved value). The Linux kernel reserves
// CID 0 (HYPERVISOR), 1 (LOCAL), 2 (HOST), and 0xffffffff (ANY) ;
// real guest VMs are assigned CIDs from 3 upward.
//
// Defined here (not in vsock_listener_linux.go) so the handler is
// portable across non-Linux builds — the listener itself is
// Linux-only, but the gating logic should compile everywhere so the
// agent's gRPC server can refuse misconfigured callers regardless
// of platform.
func IsGuestCID(cid uint32) bool {
	return cid > 2 && cid != 0xffffffff
}

// Attach reads the mandatory GuestHello frame, replies with a
// GuestHelloAck, then drains. Return path on errors :
//   - first frame not a GuestHello : InvalidArgument
//   - send Ack fails : Internal (transport dropped)
//   - subsequent recv error or EOF : nil (clean shutdown)
func (s *guestPodPlaneServer) Attach(stream guestv1.GuestPodPlane_AttachServer) error {
	// Peer-CID guard : refuse calls that didn't come from a guest VM.
	// The vsock listener stamps the peer's CID on the conn's
	// RemoteAddr() ; gRPC's peer.FromContext() surfaces that here.
	// Reserved CIDs (HYPERVISOR=0, LOCAL=1, HOST=2) mean the conn
	// arrived from the host itself (Unix socket / TCP / loopback
	// vsock) — none of those should be impersonating a guest pod.
	// On non-vsock transports peer.Addr is a *net.TCPAddr / *net.UnixAddr
	// instead of a vsockAddr ; the guard short-circuits with an error
	// in that case too — GuestPodPlane is vsock-only by design.
	if s.allowNonGuestCallers {
		// Test-only path : skip the guard so the protocol test can
		// exercise Hello/Ack/drain over bufconn.
	} else if pr, ok := peer.FromContext(stream.Context()); ok && pr.Addr != nil {
		if va, ok := pr.Addr.(interface{ CID() uint32 }); ok {
			if !IsGuestCID(va.CID()) {
				return status.Errorf(codes.PermissionDenied,
					"guest pod plane: reserved peer CID %d ; calls must originate from a guest microVM", va.CID())
			}
		} else {
			// Non-vsock transport (Unix / TCP / SSH). Refuse —
			// GuestPodPlane is vsock-only by design. Local
			// integration tests that need to exercise the handler
			// directly should call the server method itself rather
			// than through the gRPC stack.
			return status.Errorf(codes.PermissionDenied,
				"guest pod plane: caller is not on AF_VSOCK ; transport=%s", pr.Addr.Network())
		}
	}
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
	// CID enforcement on Hello. Three layers, in order :
	//
	//   1. Guest-reported vs. peer-observed cross-check.
	//      The guest reads its own CID via IOCTL_VM_SOCKETS_GET_LOCAL_CID
	//      and ships it as Hello.reported_cid. The host's peer.CID()
	//      comes from the kernel's view of the AF_VSOCK socket. The two
	//      are independent kernel observations of the same CID — they
	//      MUST agree. Disagreement = spoofing attempt or a kernel bug ;
	//      either way refuse the stream.
	//
	//   2. Registry strict-when-known.
	//      If the host's podCIDs registry has an entry for pod_id (from
	//      a previous Hello's autoregister, OR a future host-driven
	//      pre-allocation), peer.CID() MUST match it. Different CID for
	//      the same pod = a pod that's been recycled OR an impersonation.
	//
	//   3. Autoregister on first Hello.
	//      If the registry has no entry and we have a valid guest-range
	//      peer.CID(), stamp it now so future Hellos for the same pod_id
	//      enforce strict-when-known. Closes the Apple-VZ readback gap :
	//      the host couldn't pre-fill the registry (Apple's API doesn't
	//      expose the assigned CID), but now learns it from the first
	//      live stream and protects subsequent ones.
	if !s.allowNonGuestCallers && s.adp != nil {
		var peerCID uint32
		if pr, ok := peer.FromContext(stream.Context()); ok && pr.Addr != nil {
			if va, ok := pr.Addr.(interface{ CID() uint32 }); ok {
				peerCID = va.CID()
			}
		}
		// (1) reported vs. peer cross-check. Skipped when either is
		// zero (older guest builds didn't fill reported_cid, and the
		// peer accessor returns 0 for non-vsock transports — already
		// rejected by the reserved-CID guard above).
		if reported := hello.Hello.GetReportedCid(); reported != 0 && peerCID != 0 && reported != peerCID {
			return status.Errorf(codes.PermissionDenied,
				"guest pod plane: pod_id %q reported CID %d does not match peer CID %d",
				hello.Hello.PodId, reported, peerCID)
		}
		// (2) registry strict-when-known.
		if expected, known := s.adp.PodCID(hello.Hello.PodId); known {
			if peerCID != 0 && peerCID != expected {
				return status.Errorf(codes.PermissionDenied,
					"guest pod plane: pod_id %q announced from CID %d but registered as %d",
					hello.Hello.PodId, peerCID, expected)
			}
		} else if peerCID != 0 && IsGuestCID(peerCID) {
			// (3) autoregister. peerCID has already passed the
			// reserved-CID guard at function entry, so it's a valid
			// guest CID. Stamping the registry here arms layer (2) for
			// every subsequent Hello.
			s.adp.RegisterPodCID(hello.Hello.PodId, peerCID)
		}
	}
	logger.Printf("GuestPodPlane attached : pod=%s init=%s kernel=%s",
		hello.Hello.PodId, hello.Hello.InitVersion, hello.Hello.Kernel)

	// Send HelloAck carrying the operator's desired PodSpec when one
	// has been published, otherwise a minimal {pod_id} stub so the
	// guest still sees a valid ack. PodSpec lookup is opt-in : the
	// guest reconciler treats a stub-only ack as "no work, just stay
	// attached for ControlRequest frames".
	var ackSpec *guestv1.PodSpec
	if s.adp != nil {
		if spec, ok := s.adp.PodSpec(hello.Hello.PodId); ok && spec != nil {
			ackSpec = spec
		}
	}
	if ackSpec == nil {
		ackSpec = &guestv1.PodSpec{PodId: hello.Hello.PodId}
	}
	ack := &guestv1.GuestFrame{
		Body: &guestv1.GuestFrame_HelloAck{
			HelloAck: &guestv1.GuestHelloAck{Spec: ackSpec},
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
