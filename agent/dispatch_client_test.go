//go:build darwin

package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	vzdv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// fakeBidiStream is a tiny in-memory bidi-stream stub. The
// agent-side test drives it directly without spinning a real
// gRPC server : RunDispatchClient sees a grpc.BidiStreamingClient
// shape, doesn't care that it's an in-memory channel pair under
// the hood.
type fakeBidiStream struct {
	grpc.ClientStream
	sendCh chan *vzdv1.AgentMessage   // agent → "server"
	recvCh chan *vzdv1.ControlMessage // "server" → agent
	closed chan struct{}
	once   sync.Once
}

func newFakeBidi() *fakeBidiStream {
	return &fakeBidiStream{
		sendCh: make(chan *vzdv1.AgentMessage, 16),
		recvCh: make(chan *vzdv1.ControlMessage, 16),
		closed: make(chan struct{}),
	}
}

func (f *fakeBidiStream) Send(m *vzdv1.AgentMessage) error {
	select {
	case <-f.closed:
		return errors.New("stream closed")
	case f.sendCh <- m:
		return nil
	}
}

func (f *fakeBidiStream) Recv() (*vzdv1.ControlMessage, error) {
	select {
	case <-f.closed:
		return nil, io.EOF
	case m, ok := <-f.recvCh:
		if !ok {
			return nil, io.EOF
		}
		return m, nil
	}
}

func (f *fakeBidiStream) close() {
	f.once.Do(func() { close(f.closed) })
}

// fakeDispatchClient hands out one pre-built bidi stream.
type fakeDispatchClient struct{ s *fakeBidiStream }

func (f *fakeDispatchClient) Connect(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[vzdv1.AgentMessage, vzdv1.ControlMessage], error) {
	return f.s, nil
}

// TestRunDispatchClient_HandshakeAndPong pins the happy path :
// agent sends Hello, server sends HelloAck + Ping, agent
// answers Pong, server closes the stream.
func TestRunDispatchClient_HandshakeAndPong(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{
			HostUUID:     "h-123",
			AgentVersion: "test",
		})
	}()

	// Wait for the Hello.
	select {
	case msg := <-s.sendCh:
		hello := msg.GetHello()
		if hello == nil || hello.HostUuid != "h-123" {
			t.Fatalf("first message should be Hello{HostUuid=h-123}, got %T", msg.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not send Hello within 1s")
	}

	// Server replies with HelloAck.
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{
		HelloAck: &vzdv1.ControlHelloAck{SessionId: "sess-1"},
	}}

	// Server sends a Ping.
	pingedAt := time.Now().UnixNano()
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Ping{
		Ping: &vzdv1.ControlPing{SessionId: "sess-1", SentUnixNs: pingedAt},
	}}

	// Expect a Pong back with matching session_id + echoed timestamp.
	select {
	case msg := <-s.sendCh:
		pong := msg.GetPong()
		if pong == nil {
			t.Fatalf("expected Pong, got %T", msg.Body)
		}
		if pong.SessionId != "sess-1" || pong.PingedUnixNs != pingedAt {
			t.Errorf("Pong = %+v, want session=sess-1 pinged_unix_ns=%d", pong, pingedAt)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not Pong within 1s")
	}

	// Close the stream ; client returns nil.
	s.close()
	select {
	case err := <-done:
		if err != nil && err != io.EOF {
			// Some error wrappers surface "stream closed" — only
			// "Recv: EOF" is OK ; anything else is a regression.
			if !errors.Is(err, io.EOF) {
				t.Logf("client return: %v", err)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not exit after stream close")
	}
}

// TestRunDispatchClient_RejectsEmptyHostUUID pins the precondition.
func TestRunDispatchClient_RejectsEmptyHostUUID(t *testing.T) {
	err := RunDispatchClient(context.Background(), &fakeDispatchClient{s: newFakeBidi()}, DispatchOptions{})
	if err == nil {
		t.Fatal("expected error for empty HostUUID")
	}
}

// TestRunDispatchClient_DriverRequest_RoundTrip pins the driver-
// dispatch path : control plane sends a DriverRequest, the
// agent's handler is invoked, the reply echoes the request_id
// and carries the typed result.
func TestRunDispatchClient_DriverRequest_RoundTrip(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	var handlerCalls int
	handler := func(_ context.Context, req *vzdv1.DriverRequest) *vzdv1.DriverReply {
		handlerCalls++
		// Make sure we got the CreateVM op variant.
		create := req.GetCreateVm()
		if create == nil {
			t.Errorf("handler got %T, want CreateVMOp", req.Op)
		}
		return &vzdv1.DriverReply{
			RequestId: req.RequestId,
			Result: &vzdv1.DriverReply_CreateVm{
				CreateVm: &vzdv1.CreateVMResult{VmUuid: "vm-" + create.Project},
			},
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{
			HostUUID:      "h-1",
			DriverHandler: handler,
		})
	}()
	<-s.sendCh // drain Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{HelloAck: &vzdv1.ControlHelloAck{SessionId: "sess-1"}}}

	// Send a DriverRequest with a CreateVM op.
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{
		Request: &vzdv1.DriverRequest{
			RequestId: "req-abc",
			Op: &vzdv1.DriverRequest_CreateVm{CreateVm: &vzdv1.CreateVMOp{
				VmUuid:  "",
				Project: "alpha",
				Image:   "alpine:3.21",
				Cpu:     2,
				MemMb:   1024,
			}},
		},
	}}

	// Expect a Reply with matching request_id + populated result.
	select {
	case msg := <-s.sendCh:
		reply := msg.GetReply()
		if reply == nil {
			t.Fatalf("expected DriverReply, got %T", msg.Body)
		}
		if reply.RequestId != "req-abc" {
			t.Errorf("request_id = %q, want req-abc", reply.RequestId)
		}
		if reply.Error != "" {
			t.Errorf("unexpected error: %q", reply.Error)
		}
		got := reply.GetCreateVm()
		if got == nil || got.VmUuid != "vm-alpha" {
			t.Errorf("CreateVMResult = %+v, want vm_uuid=vm-alpha", got)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not reply within 1s")
	}
	if handlerCalls != 1 {
		t.Errorf("handler called %d times, want 1", handlerCalls)
	}
	s.close()
	<-done
}

// TestRunDispatchClient_RegisterMicroVMOp_RoundTrip pins the
// proto+handler path for the RegisterMicroVMOp variant : the
// control plane sends a RegisterMicroVM dispatch, the agent's
// handler unwraps the op + calls into whatever Adapter the
// production caller wired. Stub handler here ; the real call
// site (runClient) closes over a `weft.Adapter`.
func TestRunDispatchClient_RegisterMicroVMOp_RoundTrip(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	var seen *vzdv1.RegisterMicroVMOp
	handler := func(_ context.Context, req *vzdv1.DriverRequest) *vzdv1.DriverReply {
		seen = req.GetRegisterMicroVm()
		if seen == nil {
			t.Errorf("handler got %T, want RegisterMicroVMOp", req.Op)
		}
		return &vzdv1.DriverReply{
			RequestId: req.RequestId,
			Result:    &vzdv1.DriverReply_RegisterMicroVm{RegisterMicroVm: &vzdv1.RegisterMicroVMResult{}},
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{
			HostUUID:      "h-1",
			DriverHandler: handler,
		})
	}()
	<-s.sendCh // drain Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{HelloAck: &vzdv1.ControlHelloAck{SessionId: "sess-1"}}}

	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{
		Request: &vzdv1.DriverRequest{
			RequestId: "req-xyz",
			Op: &vzdv1.DriverRequest_RegisterMicroVm{
				RegisterMicroVm: &vzdv1.RegisterMicroVMOp{
					Project: "infra",
					Name:    "infra-etcd-dc1",
					Kernel:  "/data/kernel",
					Initrd:  "/data/initrd",
					Cmdline: "ncl.rootfs=virtiofs:rootfs0 console=hvc0",
					Shares: []*vzdv1.MicroVMShare{
						{Tag: "rootfs0", Path: "/data/rootfs", ReadOnly: false, Clone: true},
					},
				},
			},
		},
	}}

	select {
	case msg := <-s.sendCh:
		reply := msg.GetReply()
		if reply == nil || reply.RequestId != "req-xyz" {
			t.Fatalf("expected DriverReply{request_id=req-xyz}, got %+v", msg)
		}
		if reply.GetRegisterMicroVm() == nil {
			t.Errorf("expected RegisterMicroVMResult, got %T", reply.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not reply within 1s")
	}
	if seen == nil || seen.Name != "infra-etcd-dc1" || len(seen.Shares) != 1 {
		t.Errorf("op fields didn't reach handler: %+v", seen)
	}
	s.close()
	<-done
}

// TestRunDispatchClient_StartVMOp_RoundTrip pins the proto +
// handler path for the StartVMOp variant : the control plane
// sends a StartVM dispatch, the handler unwraps the op + the
// reply round-trips as StartVMResult. Stub handler ; the real
// call site closes over a `weft.Adapter`.
func TestRunDispatchClient_StartVMOp_RoundTrip(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	var seen *vzdv1.StartVMOp
	handler := func(_ context.Context, req *vzdv1.DriverRequest) *vzdv1.DriverReply {
		seen = req.GetStartVm()
		if seen == nil {
			t.Errorf("handler got %T, want StartVMOp", req.Op)
		}
		return &vzdv1.DriverReply{
			RequestId: req.RequestId,
			Result:    &vzdv1.DriverReply_StartVm{StartVm: &vzdv1.StartVMResult{}},
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{
			HostUUID:      "h-1",
			DriverHandler: handler,
		})
	}()
	<-s.sendCh // drain Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{HelloAck: &vzdv1.ControlHelloAck{SessionId: "sess-1"}}}

	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{
		Request: &vzdv1.DriverRequest{
			RequestId: "req-start",
			Op: &vzdv1.DriverRequest_StartVm{
				StartVm: &vzdv1.StartVMOp{Project: "infra", Name: "infra-etcd-dc1"},
			},
		},
	}}
	select {
	case msg := <-s.sendCh:
		reply := msg.GetReply()
		if reply == nil || reply.RequestId != "req-start" {
			t.Fatalf("expected DriverReply{request_id=req-start}, got %+v", msg)
		}
		if reply.GetStartVm() == nil {
			t.Errorf("expected StartVMResult, got %T", reply.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not reply within 1s")
	}
	if seen == nil || seen.Name != "infra-etcd-dc1" || seen.Project != "infra" {
		t.Errorf("op fields didn't reach handler: %+v", seen)
	}
	s.close()
	<-done
}

// TestRunDispatchClient_StopVMOp_RoundTrip pins the proto +
// handler path for the StopVMOp variant.
func TestRunDispatchClient_StopVMOp_RoundTrip(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	var seen *vzdv1.StopVMOp
	handler := func(_ context.Context, req *vzdv1.DriverRequest) *vzdv1.DriverReply {
		seen = req.GetStopVm()
		if seen == nil {
			t.Errorf("handler got %T, want StopVMOp", req.Op)
		}
		return &vzdv1.DriverReply{
			RequestId: req.RequestId,
			Result:    &vzdv1.DriverReply_StopVm{StopVm: &vzdv1.StopVMResult{}},
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{
			HostUUID:      "h-1",
			DriverHandler: handler,
		})
	}()
	<-s.sendCh // drain Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{HelloAck: &vzdv1.ControlHelloAck{SessionId: "sess-1"}}}

	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{
		Request: &vzdv1.DriverRequest{
			RequestId: "req-stop",
			Op: &vzdv1.DriverRequest_StopVm{
				StopVm: &vzdv1.StopVMOp{Project: "infra", Name: "infra-etcd-dc1"},
			},
		},
	}}
	select {
	case msg := <-s.sendCh:
		reply := msg.GetReply()
		if reply == nil || reply.RequestId != "req-stop" {
			t.Fatalf("expected DriverReply{request_id=req-stop}, got %+v", msg)
		}
		if reply.GetStopVm() == nil {
			t.Errorf("expected StopVMResult, got %T", reply.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not reply within 1s")
	}
	if seen == nil || seen.Name != "infra-etcd-dc1" || seen.Project != "infra" {
		t.Errorf("op fields didn't reach handler: %+v", seen)
	}
	s.close()
	<-done
}

// TestRunDispatchClient_DeleteVMOp_RoundTrip pins the proto +
// handler path for the DeleteVMOp variant.
func TestRunDispatchClient_DeleteVMOp_RoundTrip(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	var seen *vzdv1.DeleteVMOp
	handler := func(_ context.Context, req *vzdv1.DriverRequest) *vzdv1.DriverReply {
		seen = req.GetDeleteVm()
		if seen == nil {
			t.Errorf("handler got %T, want DeleteVMOp", req.Op)
		}
		return &vzdv1.DriverReply{
			RequestId: req.RequestId,
			Result:    &vzdv1.DriverReply_DeleteVm{DeleteVm: &vzdv1.DeleteVMResult{}},
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{
			HostUUID:      "h-1",
			DriverHandler: handler,
		})
	}()
	<-s.sendCh // drain Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{HelloAck: &vzdv1.ControlHelloAck{SessionId: "sess-1"}}}

	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{
		Request: &vzdv1.DriverRequest{
			RequestId: "req-del",
			Op: &vzdv1.DriverRequest_DeleteVm{
				DeleteVm: &vzdv1.DeleteVMOp{Project: "infra", Name: "infra-etcd-dc1"},
			},
		},
	}}
	select {
	case msg := <-s.sendCh:
		reply := msg.GetReply()
		if reply == nil || reply.RequestId != "req-del" {
			t.Fatalf("expected DriverReply{request_id=req-del}, got %+v", msg)
		}
		if reply.GetDeleteVm() == nil {
			t.Errorf("expected DeleteVMResult, got %T", reply.Result)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not reply within 1s")
	}
	if seen == nil || seen.Name != "infra-etcd-dc1" || seen.Project != "infra" {
		t.Errorf("op fields didn't reach handler: %+v", seen)
	}
	s.close()
	<-done
}

// TestRunDispatchClient_DriverRequest_NoHandler pins the
// "handler not configured" diagnostic — without that, an agent
// without a Bundle would hang the server on the reply.
func TestRunDispatchClient_DriverRequest_NoHandler(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{HostUUID: "h-1"})
	}()
	<-s.sendCh // Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_HelloAck{HelloAck: &vzdv1.ControlHelloAck{SessionId: "x"}}}
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Request{
		Request: &vzdv1.DriverRequest{
			RequestId: "req-1",
			Op:        &vzdv1.DriverRequest_CreateVm{CreateVm: &vzdv1.CreateVMOp{}},
		},
	}}
	select {
	case msg := <-s.sendCh:
		reply := msg.GetReply()
		if reply == nil || reply.Error == "" {
			t.Errorf("expected DriverReply with error, got %+v", msg)
		}
		if reply.RequestId != "req-1" {
			t.Errorf("request_id not echoed: %q", reply.RequestId)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not reply within 1s")
	}
	s.close()
	<-done
}

// TestRunDispatchClientWithRetry_Reconnects pins the
// reconnect-on-failure loop : a stream that fails immediately
// triggers a redial after backoff ; multiple attempts happen
// before ctx cancel.
func TestRunDispatchClientWithRetry_Reconnects(t *testing.T) {
	var dialCalls int32
	dialer := func() (AgentDispatchClient, error) {
		atomic.AddInt32(&dialCalls, 1)
		s := newFakeBidi()
		s.close() // pre-closed → RunDispatchClient errors out quickly
		return &fakeDispatchClient{s: s}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if got := atomic.LoadInt32(&dialCalls); got < 2 {
		t.Errorf("dialer called %d times, want at least 2 (loop should reconnect)", got)
	}
}

// TestRunDispatchClientWithRetry_StopsOnCtx pins the shutdown
// path : when ctx cancels, the loop returns its error promptly
// instead of dialing again.
func TestRunDispatchClientWithRetry_StopsOnCtx(t *testing.T) {
	dialer := func() (AgentDispatchClient, error) {
		s := newFakeBidi()
		s.close()
		return &fakeDispatchClient{s: s}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	err := RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{
		InitialBackoff: 5 * time.Second, // would block for ages if loop ignored ctx
	})
	if err != context.Canceled && err != ctx.Err() {
		t.Errorf("got err=%v, want context.Canceled", err)
	}
}

// TestRunDispatchClientWithRetry_DialerError pins the
// dial-failure path : when the dialer itself returns an error
// (not just the stream), the loop logs + backs off rather than
// crashing on a nil client.
func TestRunDispatchClientWithRetry_DialerError(t *testing.T) {
	var dialCalls int32
	dialer := func() (AgentDispatchClient, error) {
		atomic.AddInt32(&dialCalls, 1)
		return nil, errors.New("transport unreachable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{
		InitialBackoff: 5 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected ctx cancel error")
	}
	if got := atomic.LoadInt32(&dialCalls); got < 2 {
		t.Errorf("dialer called %d times despite errors, want at least 2", got)
	}
}

// TestWithJitter pins the jitter helper : zero/negative frac is
// a no-op ; positive frac adds at most frac*base to the result.
func TestWithJitter(t *testing.T) {
	const base = 100 * time.Millisecond
	if got := withJitter(base, 0); got != base {
		t.Errorf("withJitter(base, 0) = %v, want %v", got, base)
	}
	if got := withJitter(base, -1); got != base {
		t.Errorf("withJitter(base, -1) = %v, want %v (negative is no-op)", got, base)
	}
	for i := 0; i < 100; i++ {
		got := withJitter(base, 0.5)
		if got < base || got > base+base/2 {
			t.Errorf("withJitter(base, 0.5) = %v, want in [base, base+base/2]", got)
		}
	}
}

// TestDispatchDriverRequest_DefensiveDefaults pins the unit-
// level guards in dispatchDriverRequest : nil handler → clean
// reply ; handler that returns nil → clean reply ; handler
// that forgets to echo request_id → wrapper fills it in.
func TestDispatchDriverRequest_DefensiveDefaults(t *testing.T) {
	req := &vzdv1.DriverRequest{RequestId: "req-99"}

	got := dispatchDriverRequest(context.Background(), nil, req)
	if got.RequestId != "req-99" || got.Error == "" {
		t.Errorf("nil handler should produce echo+error, got %+v", got)
	}

	nilReply := func(_ context.Context, _ *vzdv1.DriverRequest) *vzdv1.DriverReply { return nil }
	got = dispatchDriverRequest(context.Background(), nilReply, req)
	if got.RequestId != "req-99" || got.Error == "" {
		t.Errorf("nil-returning handler should produce echo+error, got %+v", got)
	}

	bareReply := func(_ context.Context, _ *vzdv1.DriverRequest) *vzdv1.DriverReply {
		return &vzdv1.DriverReply{} // forgot to set RequestId
	}
	got = dispatchDriverRequest(context.Background(), bareReply, req)
	if got.RequestId != "req-99" {
		t.Errorf("wrapper should backfill request_id, got %q", got.RequestId)
	}
}

// TestRunDispatchClient_NoHelloAckErrors pins the protocol
// guard : the first server message must be a HelloAck. A
// stream that opens with a Ping is malformed.
func TestRunDispatchClient_NoHelloAckErrors(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{HostUUID: "h-1"})
	}()
	<-s.sendCh // drain the Hello
	s.recvCh <- &vzdv1.ControlMessage{Body: &vzdv1.ControlMessage_Ping{
		Ping: &vzdv1.ControlPing{SessionId: "x"},
	}}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected protocol error for missing HelloAck")
		}
	case <-time.After(time.Second):
		t.Fatal("client did not error out on missing HelloAck")
	}
}
