package agent

// dispatch_client.go is the agent-side of the bidi
// `AgentDispatch.Connect` stream. Run on a `weft agent
// --client` process : after RegisterHost succeeds the agent
// opens this stream, sends Hello, then loops reading
// ControlMessages and replying.
//
// Today the loop only handles Hello-ack + Ping/Pong ; the
// scaffold is what we need to land the driver-dispatch ops in
// follow-up slices (each new ControlMessage variant adds a
// case in the switch, the goroutine plumbing stays unchanged).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// AgentDispatchClient is the slim slice of the gRPC client this
// loop needs — just opens the bidi stream. The real generated
// `weftv1.AgentDispatchClient` satisfies this interface
// structurally so callers can pass it directly.
type AgentDispatchClient interface {
	Connect(ctx context.Context, opts ...grpc.CallOption) (grpc.BidiStreamingClient[weftv1.AgentMessage, weftv1.ControlMessage], error)
}

// DispatchOptions controls the agent-side stream lifecycle.
// Sensible defaults make the call site terse ; tests + custom
// reconnect policies hook in here.
type DispatchOptions struct {
	HostUUID     string
	AgentVersion string
	// Logger receives one line per state transition (connected,
	// disconnected, pong-received). Nil silences.
	Logger *log.Logger
	// DriverHandler dispatches an incoming DriverRequest against
	// the agent's local driver Bundle. Nil → every DriverRequest
	// is rejected with "no driver handler configured" ; the
	// production caller wires this to a closure that calls
	// `bundle.Hypervisor.<op>(...)` etc. Tests can pass a stub.
	//
	// The handler must populate `reply.RequestId` with the
	// matching `req.RequestId` so the control plane can route
	// the reply back to the awaiting goroutine.
	DriverHandler func(ctx context.Context, req *weftv1.DriverRequest) *weftv1.DriverReply
}

// RunDispatchClient opens the stream, sends Hello, then loops
// until the context cancels or the server closes the stream.
// Returns nil for clean shutdowns (context cancel + server-
// initiated close-via-EOF), an error otherwise.
//
// The caller's responsibility is reconnect-on-failure : this
// function returns when the connection drops, the caller
// decides whether to back off + redial.
func RunDispatchClient(ctx context.Context, c AgentDispatchClient, opts DispatchOptions) error {
	if opts.HostUUID == "" {
		return errors.New("RunDispatchClient: HostUUID is required")
	}
	stream, err := c.Connect(ctx)
	if err != nil {
		return fmt.Errorf("Connect: %w", err)
	}

	// Send the Hello first thing. Anything else is a protocol
	// violation on the server side.
	hello := &weftv1.AgentMessage{Body: &weftv1.AgentMessage_Hello{
		Hello: &weftv1.AgentHello{
			HostUuid:     opts.HostUUID,
			AgentVersion: opts.AgentVersion,
		},
	}}
	if err := stream.Send(hello); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	// Wait for HelloAck so we know the server registered us.
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv hello-ack: %w", err)
	}
	ack := first.GetHelloAck()
	if ack == nil {
		return fmt.Errorf("expected HelloAck, got %T", first.Body)
	}
	if opts.Logger != nil {
		opts.Logger.Printf("dispatch: connected to control plane (session=%s)", ack.SessionId)
	}

	// Receive loop. Each ControlMessage handled here ; replies
	// flow back via stream.Send. Today's variants : Ping → Pong
	// (keepalive) and DriverRequest → DriverReply (driver
	// dispatch). Future variants drop in as additional `case`
	// clauses without touching the rest.
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		switch body := msg.Body.(type) {
		case *weftv1.ControlMessage_Ping:
			pong := &weftv1.AgentMessage{Body: &weftv1.AgentMessage_Pong{
				Pong: &weftv1.AgentPong{
					SessionId:     body.Ping.SessionId,
					PingedUnixNs:  body.Ping.SentUnixNs,
					RepliedUnixNs: time.Now().UnixNano(),
				},
			}}
			if err := stream.Send(pong); err != nil {
				return fmt.Errorf("send pong: %w", err)
			}
		case *weftv1.ControlMessage_Request:
			// DriverRequest : dispatch to the configured handler,
			// echo the request_id back in the reply. A handler-
			// less agent surfaces a clean diagnostic over the
			// stream so the control plane sees the reason instead
			// of timing out.
			reply := dispatchDriverRequest(ctx, opts.DriverHandler, body.Request)
			out := &weftv1.AgentMessage{Body: &weftv1.AgentMessage_Reply{Reply: reply}}
			if err := stream.Send(out); err != nil {
				return fmt.Errorf("send driver reply: %w", err)
			}
		case *weftv1.ControlMessage_HelloAck:
			// Duplicate HelloAck (unusual but harmless) — log
			// for telemetry, keep the connection.
			if opts.Logger != nil {
				opts.Logger.Printf("dispatch: duplicate HelloAck (session=%s)", body.HelloAck.SessionId)
			}
		}
	}
}

// RetryOptions configures RunDispatchClientWithRetry's backoff
// loop. Zero values fall back to sensible defaults so a caller
// that just wants "retry forever with exp backoff" writes
// `RunDispatchClientWithRetry(ctx, c, opts, RetryOptions{})`.
type RetryOptions struct {
	// InitialBackoff is the first sleep after a failed attempt.
	// Default 1s.
	InitialBackoff time.Duration
	// MaxBackoff caps the exponential growth. Default 30s.
	MaxBackoff time.Duration
	// JitterFrac adds up to this fraction of the current backoff
	// as random extra sleep — prevents a thundering herd of
	// reconnects when many agents lose a shared control-plane.
	// Default 0.25 (25%).
	JitterFrac float64
}

// RunDispatchClientWithRetry wraps RunDispatchClient in a loop
// that redials on every failure with exponential backoff +
// jitter. Returns only when `ctx` cancels — every other failure
// path (stream EOF, transport error, Hello rejection) is treated
// as transient and retried.
//
// Backoff resets to InitialBackoff each time the stream stays up
// long enough to receive a HelloAck — meaning a quick disconnect
// after a stable session doesn't penalise the next reconnect.
//
// `dialer` returns a fresh client view of the underlying gRPC
// connection — typical caller passes the same generated
// `weftv1.AgentDispatchClient` every time (the *grpc.ClientConn
// handles transport reconnects underneath), but tests can
// substitute a stub that returns errors / different streams.
func RunDispatchClientWithRetry(ctx context.Context, dialer func() (AgentDispatchClient, error), opts DispatchOptions, retry RetryOptions) error {
	if retry.InitialBackoff <= 0 {
		retry.InitialBackoff = time.Second
	}
	if retry.MaxBackoff <= 0 {
		retry.MaxBackoff = 30 * time.Second
	}
	if retry.JitterFrac < 0 {
		retry.JitterFrac = 0
	}
	backoff := retry.InitialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := dialer()
		if err != nil {
			if opts.Logger != nil {
				opts.Logger.Printf("dispatch: dial failed (%v) ; retry in %s", err, withJitter(backoff, retry.JitterFrac))
			}
		} else {
			runStarted := time.Now()
			err = RunDispatchClient(ctx, c, opts)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Reset the backoff only if the session lasted past
			// the first interval — a stream that drops in <1s
			// is most likely a config / auth issue and we don't
			// want to spin redialing.
			if time.Since(runStarted) > retry.InitialBackoff {
				backoff = retry.InitialBackoff
			}
			if opts.Logger != nil {
				opts.Logger.Printf("dispatch: stream ended (%v) ; retry in %s", err, withJitter(backoff, retry.JitterFrac))
			}
		}
		sleep := withJitter(backoff, retry.JitterFrac)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
		backoff *= 2
		if backoff > retry.MaxBackoff {
			backoff = retry.MaxBackoff
		}
	}
}

// withJitter returns base + up to base*frac extra. Uses
// math/rand which is fine here — we want unpredictability for
// load-spreading, not cryptographic randomness.
func withJitter(base time.Duration, frac float64) time.Duration {
	if frac <= 0 {
		return base
	}
	extra := time.Duration(float64(base) * frac * rand.Float64())
	return base + extra
}

// dispatchDriverRequest runs the agent's configured handler and
// guarantees a non-nil reply with a matching request_id ; nil
// handler / nil-returning handler surface a clean error rather
// than hanging the server.
func dispatchDriverRequest(ctx context.Context, handler func(context.Context, *weftv1.DriverRequest) *weftv1.DriverReply, req *weftv1.DriverRequest) *weftv1.DriverReply {
	if handler == nil {
		return &weftv1.DriverReply{RequestId: req.RequestId, Error: "no driver handler configured on this agent"}
	}
	reply := handler(ctx, req)
	if reply == nil {
		return &weftv1.DriverReply{RequestId: req.RequestId, Error: "driver handler returned nil reply"}
	}
	// Defensive : enforce the request_id echo so a buggy
	// handler can't silently break correlation.
	if reply.RequestId == "" {
		reply.RequestId = req.RequestId
	}
	return reply
}
