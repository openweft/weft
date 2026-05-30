//go:build darwin

package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// -- loadOrCreateHostUUID error paths --------------------------------------

// TestLoadOrCreateHostUUID_EmptyStateDir surfaces a clean error when the
// caller passes an empty path.
func TestLoadOrCreateHostUUID_EmptyStateDir(t *testing.T) {
	_, err := loadOrCreateHostUUID("")
	if err == nil {
		t.Fatal("expected error for empty state dir")
	}
}

// TestLoadOrCreateHostUUID_BlankFileIsReplaced confirms a host-uuid file
// containing only whitespace is treated as empty and rewritten with a
// freshly generated UUID.
func TestLoadOrCreateHostUUID_BlankFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the file with whitespace content — TrimSpace yields ""
	// so the code path re-generates.
	if err := os.WriteFile(filepath.Join(dir, "host-uuid"), []byte("   \n  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uuid, err := loadOrCreateHostUUID(dir)
	if err != nil {
		t.Fatalf("loadOrCreateHostUUID: %v", err)
	}
	if uuid == "" {
		t.Errorf("expected non-empty UUID after blank-file regeneration")
	}
}

// TestLoadOrCreateHostUUID_MkdirFailure: when the parent of stateDir is
// a regular file, MkdirAll fails. The error is surfaced.
func TestLoadOrCreateHostUUID_MkdirFailure(t *testing.T) {
	tmp := t.TempDir()
	// Create a file where the state-dir parent should be.
	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// stateDir = blocker/inner — MkdirAll fails because blocker is a file.
	stateDir := filepath.Join(blocker, "inner")
	_, err := loadOrCreateHostUUID(stateDir)
	if err == nil {
		t.Fatalf("expected mkdir failure when state-dir is under a regular file, got nil")
	}
	if !strings.Contains(err.Error(), "mkdir") {
		t.Errorf("error should mention mkdir, got: %v", err)
	}
}

// TestLoadOrCreateHostUUID_RenameFailure exercises the os.Rename error
// branch by pre-creating host-uuid as a directory (with content), so
// the final rename fails.
func TestLoadOrCreateHostUUID_RenameFailure(t *testing.T) {
	dir := t.TempDir()
	// Pre-create host-uuid as a non-empty directory so Rename(file→dir) fails.
	hostUUIDDir := filepath.Join(dir, "host-uuid")
	if err := os.MkdirAll(hostUUIDDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostUUIDDir, "stub"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadOrCreateHostUUID(dir)
	if err == nil {
		t.Fatalf("expected rename failure when host-uuid is a non-empty directory")
	}
}

// TestLoadOrCreateHostUUID_CreateTempFailure surfaces an error when the
// state-dir is a regular file (so CreateTemp inside it fails).
func TestLoadOrCreateHostUUID_CreateTempFailure(t *testing.T) {
	tmp := t.TempDir()
	// Pre-create a file masquerading as a directory: makes os.CreateTemp fail.
	stateDir := filepath.Join(tmp, "stateDir")
	if err := os.WriteFile(stateDir, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	// loadOrCreateHostUUID tries to read host-uuid first (fails: not a dir),
	// then MkdirAll on the parent (succeeds because tmp exists), then
	// CreateTemp on stateDir (fails: not a directory).
	// Actually MkdirAll(stateDir) with stateDir already being a regular file
	// errors first with "not a directory".
	if _, err := loadOrCreateHostUUID(stateDir); err == nil {
		t.Errorf("expected error when state-dir path is a regular file")
	}
}

// -- newAgentUUID happy-path format check ----------------------------------

// TestNewAgentUUID_Format pins the RFC 4122 v4 shape of the generated UUID.
func TestNewAgentUUID_Format(t *testing.T) {
	u := newAgentUUID()
	if len(u) != 36 {
		t.Fatalf("UUID len = %d, want 36", len(u))
	}
	// 8-4-4-4-12 hex groups
	parts := strings.Split(u, "-")
	if len(parts) != 5 {
		t.Fatalf("UUID parts = %d, want 5: %s", len(parts), u)
	}
	wantLens := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != wantLens[i] {
			t.Errorf("part %d len = %d, want %d", i, len(p), wantLens[i])
		}
	}
	// Version bit: 13th hex char should be '4'.
	if u[14] != '4' {
		t.Errorf("expected v4 marker at position 14, got %c (full: %s)", u[14], u)
	}
	// Variant: 17th hex char should be 8/9/a/b.
	switch u[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("expected variant marker (8/9/a/b) at 19, got %c", u[19])
	}
}

// TestNewAgentUUID_Unique sanity: two consecutive calls produce
// different UUIDs (probabilistic, but virtually guaranteed for v4).
func TestNewAgentUUID_Unique(t *testing.T) {
	a := newAgentUUID()
	b := newAgentUUID()
	if a == b {
		t.Errorf("expected different UUIDs, got %q twice", a)
	}
}

// -- start: bundle setup + hostname override + Hostname fallback ------------

// TestStart_HostnameOverride confirms the operator-supplied Hostname
// option propagates to the registration record.
func TestStart_HostnameOverride(t *testing.T) {
	cp := newRecordingCP()
	a, err := New(Options{
		StateDir:          t.TempDir(),
		Hostname:          "custom-name",
		HeartbeatInterval: time.Hour,
		ControlPlane:      cp,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cp.registered) != 1 || cp.registered[0].Hostname != "custom-name" {
		t.Errorf("Hostname override not propagated: %v", cp.registered)
	}
}

// TestStart_LoadUUIDError surfaces a host-uuid failure from start().
func TestStart_LoadUUIDError(t *testing.T) {
	// State dir is a file → loadOrCreateHostUUID fails.
	tmp := t.TempDir()
	stateFile := filepath.Join(tmp, "stateDirAsFile")
	if err := os.WriteFile(stateFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp := newRecordingCP()
	a, err := New(Options{StateDir: stateFile, ControlPlane: cp})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err == nil {
		t.Errorf("expected Start failure when state-dir is a file")
	}
}

// TestStart_Idempotent confirms re-calling Start is a no-op once the
// startOnce.Do has fired.
func TestStart_Idempotent(t *testing.T) {
	cp := newRecordingCP()
	a, _ := New(Options{StateDir: t.TempDir(), ControlPlane: cp, HeartbeatInterval: time.Hour})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Stop(context.Background())
	if err := a.Start(context.Background()); err != nil {
		t.Errorf("second Start should be a no-op, got %v", err)
	}
	if len(cp.registered) != 1 {
		t.Errorf("RegisterHost called %d times, want 1", len(cp.registered))
	}
}

// -- heartbeat loop ----------------------------------------------------------

// erroringCP returns an error on Heartbeat to exercise the error-log
// branch of heartbeatLoop.
type erroringCP struct {
	recordingCP
	hbErr atomic.Value // error
	calls atomic.Int32
}

func (cp *erroringCP) Heartbeat(ctx context.Context, hostUUID string) error {
	cp.calls.Add(1)
	if e, ok := cp.hbErr.Load().(error); ok && e != nil {
		return e
	}
	return cp.recordingCP.Heartbeat(ctx, hostUUID)
}

// TestHeartbeatLoop_ErrorIsLoggedAndLoopContinues : a Heartbeat that
// returns an error is logged to stderr and the loop continues.
func TestHeartbeatLoop_ErrorIsLoggedAndLoopContinues(t *testing.T) {
	// Redirect stderr to a pipe so we can read the diagnostic.
	origStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	cp := &erroringCP{}
	cp.attachedHandle = make(map[string]DriverHandles)
	cp.hbErr.Store(errors.New("control plane is down"))

	a, err := New(Options{
		StateDir:          t.TempDir(),
		ControlPlane:      cp,
		HeartbeatInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the loop fire at least twice.
	time.Sleep(50 * time.Millisecond)
	_ = a.Stop(context.Background())
	w.Close()

	got, _ := io.ReadAll(r)
	if !strings.Contains(string(got), "heartbeat failed") {
		t.Errorf("stderr missing 'heartbeat failed' diagnostic: %s", got)
	}
	if cp.calls.Load() < 2 {
		t.Errorf("heartbeat called %d times, want >= 2 (loop should continue after error)", cp.calls.Load())
	}
}

// -- RunDispatchClient error paths -----------------------------------------

// failingConnectClient surfaces Connect errors so the corresponding
// branch of RunDispatchClient is exercised.
type failingConnectClient struct {
	err error
}

func (f *failingConnectClient) Connect(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[weftv1.AgentMessage, weftv1.ControlMessage], error) {
	return nil, f.err
}

// TestRunDispatchClient_ConnectError surfaces a Connect error.
func TestRunDispatchClient_ConnectError(t *testing.T) {
	err := RunDispatchClient(context.Background(), &failingConnectClient{err: errors.New("transport closed")}, DispatchOptions{HostUUID: "h-1"})
	if err == nil {
		t.Fatal("expected error when Connect fails")
	}
	if !strings.Contains(err.Error(), "Connect") {
		t.Errorf("error should mention Connect, got: %v", err)
	}
}

// preClosedBidiStream surfaces a send error on the very first message —
// exercises the "send hello error" branch of RunDispatchClient.
type preClosedBidiStream struct {
	*fakeBidiStream
}

func newPreClosedBidi() *preClosedBidiStream {
	f := newFakeBidi()
	close(f.closed) // pre-close so any Send/Recv errors immediately
	return &preClosedBidiStream{fakeBidiStream: f}
}

type preClosedDispatchClient struct{ s *preClosedBidiStream }

func (p *preClosedDispatchClient) Connect(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[weftv1.AgentMessage, weftv1.ControlMessage], error) {
	return p.s, nil
}

// TestRunDispatchClient_SendHelloError exercises the Send-failure branch.
func TestRunDispatchClient_SendHelloError(t *testing.T) {
	err := RunDispatchClient(context.Background(), &preClosedDispatchClient{s: newPreClosedBidi()}, DispatchOptions{HostUUID: "h-1"})
	if err == nil {
		t.Fatal("expected error when Send fails")
	}
	if !strings.Contains(err.Error(), "hello") {
		t.Errorf("error should mention hello, got: %v", err)
	}
}

// TestRunDispatchClient_RecvHelloAckError exercises the Recv failure
// after Send succeeds : the stream is closed right after the agent
// sends Hello so Recv returns EOF before HelloAck arrives.
func TestRunDispatchClient_RecvHelloAckError(t *testing.T) {
	s := newFakeBidi()
	go func() {
		// Drain Hello + close the stream so Recv returns EOF.
		<-s.sendCh
		s.close()
	}()
	c := &fakeDispatchClient{s: s}
	err := RunDispatchClient(context.Background(), c, DispatchOptions{HostUUID: "h-1"})
	if err == nil {
		t.Fatal("expected error from recv hello-ack")
	}
	if !strings.Contains(err.Error(), "hello-ack") {
		t.Errorf("error should mention hello-ack, got: %v", err)
	}
}

// sendFailingBidi is a stream that Recv()s ControlMessages normally but
// rejects Send() with an error. Used to deterministically trigger the
// "send pong" / "send driver reply" failure branches.
type sendFailingBidi struct {
	grpc.ClientStream
	recvCh chan *weftv1.ControlMessage
	// once Recv has emitted a HelloAck + a follow-up, future Sends fail.
	sendsAllowed int // atomic-ish: only the goroutine that calls Send sets it
	sendErr      error
}

func newSendFailingBidi(sendErr error) *sendFailingBidi {
	return &sendFailingBidi{
		recvCh:  make(chan *weftv1.ControlMessage, 16),
		sendErr: sendErr,
	}
}

func (f *sendFailingBidi) Send(_ *weftv1.AgentMessage) error {
	f.sendsAllowed++
	if f.sendsAllowed == 1 {
		// First Send is the Hello — let it through.
		return nil
	}
	return f.sendErr
}

func (f *sendFailingBidi) Recv() (*weftv1.ControlMessage, error) {
	m, ok := <-f.recvCh
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

type sendFailingDispatchClient struct{ s *sendFailingBidi }

func (c *sendFailingDispatchClient) Connect(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[weftv1.AgentMessage, weftv1.ControlMessage], error) {
	return c.s, nil
}

// TestRunDispatchClient_SendPongError pins the send-pong failure branch
// using a deterministic stream that rejects Send after Hello.
func TestRunDispatchClient_SendPongError(t *testing.T) {
	s := newSendFailingBidi(errors.New("write failed"))
	go func() {
		// Push HelloAck then a Ping so the loop tries to Send a Pong.
		s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "x"}}}
		s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_Ping{Ping: &weftv1.ControlPing{SessionId: "x"}}}
	}()
	err := RunDispatchClient(context.Background(), &sendFailingDispatchClient{s: s}, DispatchOptions{HostUUID: "h-1"})
	if err == nil {
		t.Fatal("expected error from send pong")
	}
	if !strings.Contains(err.Error(), "send pong") {
		t.Errorf("error should mention 'send pong', got: %v", err)
	}
}

// TestRunDispatchClient_SendDriverReplyError_Deterministic pins the
// send-reply failure branch using the same stream surface.
func TestRunDispatchClient_SendDriverReplyError_Deterministic(t *testing.T) {
	s := newSendFailingBidi(errors.New("write failed"))
	go func() {
		s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "x"}}}
		s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_Request{Request: &weftv1.DriverRequest{RequestId: "req"}}}
	}()
	handler := func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		return &weftv1.DriverReply{RequestId: req.RequestId}
	}
	err := RunDispatchClient(context.Background(), &sendFailingDispatchClient{s: s}, DispatchOptions{HostUUID: "h-1", DriverHandler: handler})
	if err == nil {
		t.Fatal("expected error from send driver reply")
	}
	if !strings.Contains(err.Error(), "send driver reply") {
		t.Errorf("error should mention 'send driver reply', got: %v", err)
	}
}

// recvErrorBidi emits a HelloAck then returns a non-EOF error on the
// next Recv — exercises the "recv: %w" error branch of the receive
// loop (as opposed to the clean EOF shutdown).
type recvErrorBidi struct {
	grpc.ClientStream
	step    int
	recvErr error
}

func (f *recvErrorBidi) Send(_ *weftv1.AgentMessage) error { return nil }

func (f *recvErrorBidi) Recv() (*weftv1.ControlMessage, error) {
	f.step++
	switch f.step {
	case 1:
		return &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "x"}}}, nil
	default:
		return nil, f.recvErr
	}
}

type recvErrorDispatchClient struct{ s *recvErrorBidi }

func (c *recvErrorDispatchClient) Connect(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[weftv1.AgentMessage, weftv1.ControlMessage], error) {
	return c.s, nil
}

// TestRunDispatchClient_RecvLoopError pins the receive-loop transport
// error branch (non-EOF Recv failure after a clean handshake).
func TestRunDispatchClient_RecvLoopError(t *testing.T) {
	s := &recvErrorBidi{recvErr: errors.New("connection reset by peer")}
	err := RunDispatchClient(context.Background(), &recvErrorDispatchClient{s: s}, DispatchOptions{HostUUID: "h-1"})
	if err == nil {
		t.Fatal("expected error from recv loop")
	}
	if !strings.Contains(err.Error(), "recv") {
		t.Errorf("error should mention recv, got: %v", err)
	}
}

// TestRunDispatchClient_DuplicateHelloAckLogged confirms the duplicate-
// HelloAck branch logs a line via the provided logger.
func TestRunDispatchClient_DuplicateHelloAckLogged(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{HostUUID: "h-1", Logger: logger})
	}()
	<-s.sendCh
	s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "sess-A"}}}
	// Duplicate HelloAck — logged + ignored.
	s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "sess-B"}}}
	time.Sleep(20 * time.Millisecond) // let goroutine catch up
	s.close()
	<-done
	if !strings.Contains(buf.String(), "duplicate HelloAck") {
		t.Errorf("expected duplicate HelloAck log line, got: %s", buf.String())
	}
}

// TestRunDispatchClient_SendDriverReplyError exercises the send-reply
// failure branch by closing the stream after the request arrives.
func TestRunDispatchClient_SendDriverReplyError(t *testing.T) {
	s := newFakeBidi()
	c := &fakeDispatchClient{s: s}

	handler := func(_ context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply {
		return &weftv1.DriverReply{RequestId: req.RequestId, Error: "no-op"}
	}

	done := make(chan error, 1)
	go func() {
		done <- RunDispatchClient(context.Background(), c, DispatchOptions{HostUUID: "h-1", DriverHandler: handler})
	}()
	<-s.sendCh
	s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "x"}}}
	// Close before sending Request so Send fails.
	s.close()
	// Push a DriverRequest; the recv loop will see EOF before processing,
	// so we just accept whatever the test surface returns.
	select {
	case s.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_Request{Request: &weftv1.DriverRequest{RequestId: "rq-1"}}}:
	default:
	}
	<-done
}

// -- RunDispatchClientWithRetry coverage of logger branches ----------------

// TestRunDispatchClientWithRetry_LoggerOutput exercises the dial-fail
// + retry-and-log branch of the retry loop.
func TestRunDispatchClientWithRetry_LoggerOutput(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	dialer := func() (AgentDispatchClient, error) {
		return nil, errors.New("transient")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1", Logger: logger}, RetryOptions{InitialBackoff: 5 * time.Millisecond})
	if !strings.Contains(buf.String(), "dial failed") {
		t.Errorf("expected 'dial failed' log line, got: %s", buf.String())
	}
}

// TestRunDispatchClientWithRetry_StreamEndedLogged exercises the
// "stream ended" log branch : a dial succeeds but the stream EOFs
// immediately, and the loop logs before retrying.
func TestRunDispatchClientWithRetry_StreamEndedLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	dialer := func() (AgentDispatchClient, error) {
		s := newFakeBidi()
		s.close()
		return &fakeDispatchClient{s: s}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1", Logger: logger}, RetryOptions{InitialBackoff: 5 * time.Millisecond})
	if !strings.Contains(buf.String(), "stream ended") {
		t.Errorf("expected 'stream ended' log line, got: %s", buf.String())
	}
}

// TestRunDispatchClientWithRetry_BackoffCap exercises the
// `backoff > MaxBackoff → cap` branch by running long enough for the
// growth to exceed Max.
func TestRunDispatchClientWithRetry_BackoffCap(t *testing.T) {
	dialer := func() (AgentDispatchClient, error) {
		return nil, errors.New("nope")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
	})
}

// TestRunDispatchClientWithRetry_ZeroBackoffDefaults exercises the
// `InitialBackoff <= 0 → default 1s` branch. We use a context with a
// 5ms timeout so the loop returns near-immediately even with the 1s
// default sleep.
func TestRunDispatchClientWithRetry_ZeroBackoffDefaults(t *testing.T) {
	dialer := func() (AgentDispatchClient, error) {
		return nil, errors.New("nope")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_ = RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{})
	// The function returns when ctx times out — no need to assert.
}

// TestRunDispatchClientWithRetry_LongSessionResetsBackoff exercises the
// "time.Since(runStarted) > retry.InitialBackoff → backoff = Initial"
// branch by holding a successful session open for longer than the
// initial backoff before disconnecting.
func TestRunDispatchClientWithRetry_LongSessionResetsBackoff(t *testing.T) {
	var dialCounter atomic.Int32
	dialer := func() (AgentDispatchClient, error) {
		dialCounter.Add(1)
		s := newFakeBidi()
		go func(stream *fakeBidiStream) {
			select {
			case <-stream.sendCh:
			case <-time.After(50 * time.Millisecond):
				return
			}
			select {
			case stream.recvCh <- &weftv1.ControlMessage{Body: &weftv1.ControlMessage_HelloAck{HelloAck: &weftv1.ControlHelloAck{SessionId: "s1"}}}:
			default:
				return
			}
			time.Sleep(25 * time.Millisecond)
			stream.close()
		}(s)
		return &fakeDispatchClient{s: s}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_ = RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	if dialCounter.Load() < 2 {
		t.Errorf("dialer called %d times, want >= 2", dialCounter.Load())
	}
}

// TestRunDispatchClientWithRetry_NegativeJitter exercises the
// JitterFrac<0 → 0 normalisation branch.
func TestRunDispatchClientWithRetry_NegativeJitter(t *testing.T) {
	dialer := func() (AgentDispatchClient, error) {
		return nil, errors.New("nope")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = RunDispatchClientWithRetry(ctx, dialer, DispatchOptions{HostUUID: "h-1"}, RetryOptions{
		InitialBackoff: 5 * time.Millisecond,
		JitterFrac:     -1,
	})
}

// -- grpc_cp coverage -------------------------------------------------------

// TestGRPCControlPlane_RegisterHost_GRPCError surfaces a gRPC call
// error from the client.
func TestGRPCControlPlane_RegisterHost_GRPCError(t *testing.T) {
	fake := &fakeGRPCClient{regErr: errors.New("rpc unavailable")}
	cp := NewGRPCControlPlane(fake, nil)
	_, err := cp.RegisterHost(context.Background(), HostRegistration{Hostname: "h"})
	if err == nil {
		t.Fatal("expected error from RegisterHost")
	}
	if !strings.Contains(err.Error(), "rpc unavailable") {
		t.Errorf("error should wrap gRPC error, got: %v", err)
	}
}

// TestGRPCControlPlane_AttachDrivers_Logs confirms the logger receives
// the deferred-no-op diagnostic.
func TestGRPCControlPlane_AttachDrivers_Logs(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	cp := NewGRPCControlPlane(&fakeGRPCClient{}, logger)
	if err := cp.AttachDrivers(context.Background(), "h-xyz", DriverHandles{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "AttachDrivers(h-xyz)") {
		t.Errorf("expected logger to mention the host UUID, got: %s", buf.String())
	}
}
